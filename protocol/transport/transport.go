package transport

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"database/sql"
	"encoding/hex"
	"math/big"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"go.uber.org/zap"

	"github.com/status-im/status-go/eth-node/crypto"
	"github.com/status-im/status-go/eth-node/types"
)

var (
	// ErrNoMailservers returned if there is no configured mailservers that can be used.
	ErrNoMailservers = errors.New("no configured mailservers")
)

type transportKeysManager struct {
	waku types.Waku

	// Identity of the current user.
	privateKey *ecdsa.PrivateKey

	passToSymKeyMutex sync.RWMutex
	passToSymKeyCache map[string]string
}

func (m *transportKeysManager) AddOrGetKeyPair(priv *ecdsa.PrivateKey) (string, error) {
	// caching is handled in waku
	return m.waku.AddKeyPair(priv)
}

func (m *transportKeysManager) AddOrGetSymKeyFromPassword(password string) (string, error) {
	m.passToSymKeyMutex.Lock()
	defer m.passToSymKeyMutex.Unlock()

	if val, ok := m.passToSymKeyCache[password]; ok {
		return val, nil
	}

	id, err := m.waku.AddSymKeyFromPassword(password)
	if err != nil {
		return id, err
	}

	m.passToSymKeyCache[password] = id

	return id, nil
}

func (m *transportKeysManager) RawSymKey(id string) ([]byte, error) {
	return m.waku.GetSymKey(id)
}

type Option func(*Transport) error

// Transport is a transport based on Whisper service.
type Transport struct {
	waku        types.Waku
	api         types.PublicWakuAPI // only PublicWakuAPI implements logic to send messages
	keysManager *transportKeysManager
	filters     *FiltersManager
	logger      *zap.Logger
	cache       *ProcessedMessageIDsCache

	mailservers      []string
	envelopesMonitor *EnvelopesMonitor
	quit             chan struct{}
}

// NewTransport returns a new Transport.
// TODO: leaving a chat should verify that for a given public key
//       there are no other chats. It may happen that we leave a private chat
//       but still have a public chat for a given public key.
func NewTransport(
	waku types.Waku,
	privateKey *ecdsa.PrivateKey,
	db *sql.DB,
	mailservers []string,
	envelopesMonitorConfig *EnvelopesMonitorConfig,
	logger *zap.Logger,
	opts ...Option,
) (*Transport, error) {
	filtersManager, err := NewFiltersManager(newSQLitePersistence(db), waku, privateKey, logger)
	if err != nil {
		return nil, err
	}

	var envelopesMonitor *EnvelopesMonitor
	if envelopesMonitorConfig != nil {
		envelopesMonitor = NewEnvelopesMonitor(waku, *envelopesMonitorConfig)
		envelopesMonitor.Start()
	}

	var api types.PublicWhisperAPI
	if waku != nil {
		api = waku.PublicWakuAPI()
	}
	t := &Transport{
		waku:             waku,
		api:              api,
		cache:            NewProcessedMessageIDsCache(db),
		envelopesMonitor: envelopesMonitor,
		quit:             make(chan struct{}),
		keysManager: &transportKeysManager{
			waku:              waku,
			privateKey:        privateKey,
			passToSymKeyCache: make(map[string]string),
		},
		filters:     filtersManager,
		mailservers: mailservers,
		logger:      logger.With(zap.Namespace("Transport")),
	}

	for _, opt := range opts {
		if err := opt(t); err != nil {
			return nil, err
		}
	}

	t.cleanFiltersLoop()

	return t, nil
}

func (t *Transport) InitFilters(chatIDs []string, publicKeys []*ecdsa.PublicKey) ([]*Filter, error) {
	return t.filters.Init(chatIDs, publicKeys)
}

func (t *Transport) InitPublicFilters(chatIDs []string) ([]*Filter, error) {
	return t.filters.InitPublicFilters(chatIDs)
}

func (t *Transport) Filters() []*Filter {
	return t.filters.Filters()
}

func (t *Transport) FilterByChatID(chatID string) *Filter {
	return t.filters.FilterByChatID(chatID)
}

func (t *Transport) LoadFilters(filters []*Filter) ([]*Filter, error) {
	return t.filters.InitWithFilters(filters)
}

func (t *Transport) InitCommunityFilters(pks []*ecdsa.PrivateKey) ([]*Filter, error) {
	return t.filters.InitCommunityFilters(pks)
}

func (t *Transport) RemoveFilters(filters []*Filter) error {
	return t.filters.Remove(filters...)
}

func (t *Transport) RemoveFilterByChatID(chatID string) (*Filter, error) {
	return t.filters.RemoveFilterByChatID(chatID)
}

func (t *Transport) ResetFilters() error {
	return t.filters.Reset()
}

func (t *Transport) ProcessNegotiatedSecret(secret types.NegotiatedSecret) (*Filter, error) {
	filter, err := t.filters.LoadNegotiated(secret)
	if err != nil {
		return nil, err
	}
	return filter, nil
}

func (t *Transport) JoinPublic(chatID string) (*Filter, error) {
	return t.filters.LoadPublic(chatID)
}

func (t *Transport) LeavePublic(chatID string) error {
	chat := t.filters.Filter(chatID)
	if chat != nil {
		return nil
	}
	return t.filters.Remove(chat)
}

func (t *Transport) JoinPrivate(publicKey *ecdsa.PublicKey) (*Filter, error) {
	return t.filters.LoadContactCode(publicKey)
}

func (t *Transport) LeavePrivate(publicKey *ecdsa.PublicKey) error {
	filters := t.filters.FiltersByPublicKey(publicKey)
	return t.filters.Remove(filters...)
}

func (t *Transport) JoinGroup(publicKeys []*ecdsa.PublicKey) ([]*Filter, error) {
	var filters []*Filter
	for _, pk := range publicKeys {
		f, err := t.filters.LoadContactCode(pk)
		if err != nil {
			return nil, err
		}
		filters = append(filters, f)

	}
	return filters, nil
}

func (t *Transport) LeaveGroup(publicKeys []*ecdsa.PublicKey) error {
	for _, publicKey := range publicKeys {
		filters := t.filters.FiltersByPublicKey(publicKey)
		if err := t.filters.Remove(filters...); err != nil {
			return err
		}
	}
	return nil
}

func (t *Transport) RetrieveRawAll() (map[Filter][]*types.Message, error) {
	result := make(map[Filter][]*types.Message)

	allFilters := t.filters.Filters()
	for _, filter := range allFilters {
		// Don't pull from filters we don't listen to
		if !filter.Listen {
			continue
		}
		msgs, err := t.api.GetFilterMessages(filter.FilterID)
		if err != nil {
			t.logger.Warn("failed to fetch messages", zap.Error(err))
			continue
		}
		if len(msgs) == 0 {
			continue
		}

		ids := make([]string, len(msgs))
		for i := range msgs {
			id := types.EncodeHex(msgs[i].Hash)
			ids[i] = id
		}

		hits, err := t.cache.Hits(ids)
		if err != nil {
			t.logger.Error("failed to check messages exists", zap.Error(err))
			return nil, err
		}

		for i := range msgs {
			// Exclude anything that is a cache hit
			if !hits[types.EncodeHex(msgs[i].Hash)] {
				result[*filter] = append(result[*filter], msgs[i])
			}
		}

	}

	return result, nil
}

// SendPublic sends a new message using the Whisper service.
// For public filters, chat name is used as an ID as well as
// a topic.
func (t *Transport) SendPublic(ctx context.Context, newMessage *types.NewMessage, chatName string) ([]byte, error) {
	if err := t.addSig(newMessage); err != nil {
		return nil, err
	}

	filter, err := t.filters.LoadPublic(chatName)
	if err != nil {
		return nil, err
	}

	newMessage.SymKeyID = filter.SymKeyID
	newMessage.Topic = filter.Topic

	return t.api.Post(ctx, *newMessage)
}

func (t *Transport) SendPrivateWithSharedSecret(ctx context.Context, newMessage *types.NewMessage, publicKey *ecdsa.PublicKey, secret []byte) ([]byte, error) {
	if err := t.addSig(newMessage); err != nil {
		return nil, err
	}

	filter, err := t.filters.LoadNegotiated(types.NegotiatedSecret{
		PublicKey: publicKey,
		Key:       secret,
	})
	if err != nil {
		return nil, err
	}

	newMessage.SymKeyID = filter.SymKeyID
	newMessage.Topic = filter.Topic
	newMessage.PublicKey = nil

	return t.api.Post(ctx, *newMessage)
}

func (t *Transport) SendPrivateWithPartitioned(ctx context.Context, newMessage *types.NewMessage, publicKey *ecdsa.PublicKey) ([]byte, error) {
	if err := t.addSig(newMessage); err != nil {
		return nil, err
	}

	filter, err := t.filters.LoadPartitioned(publicKey, t.keysManager.privateKey, false)
	if err != nil {
		return nil, err
	}

	newMessage.Topic = filter.Topic
	newMessage.PublicKey = crypto.FromECDSAPub(publicKey)

	return t.api.Post(ctx, *newMessage)
}

func (t *Transport) SendPrivateOnPersonalTopic(ctx context.Context, newMessage *types.NewMessage, publicKey *ecdsa.PublicKey) ([]byte, error) {
	if err := t.addSig(newMessage); err != nil {
		return nil, err
	}

	filter, err := t.filters.LoadPersonal(publicKey, t.keysManager.privateKey, false)
	if err != nil {
		return nil, err
	}

	newMessage.Topic = filter.Topic
	newMessage.PublicKey = crypto.FromECDSAPub(publicKey)

	return t.api.Post(ctx, *newMessage)
}

func (t *Transport) LoadKeyFilters(key *ecdsa.PrivateKey) (*Filter, error) {
	return t.filters.LoadEphemeral(&key.PublicKey, key, true)
}

func (t *Transport) SendCommunityMessage(ctx context.Context, newMessage *types.NewMessage, publicKey *ecdsa.PublicKey) ([]byte, error) {
	if err := t.addSig(newMessage); err != nil {
		return nil, err
	}

	// We load the filter to make sure we can post on it
	filter, err := t.filters.LoadPublic(PubkeyToHex(publicKey)[2:])
	if err != nil {
		return nil, err
	}

	newMessage.Topic = filter.Topic
	newMessage.PublicKey = crypto.FromECDSAPub(publicKey)

	t.logger.Debug("SENDING message", zap.Binary("topic", filter.Topic[:]))

	return t.api.Post(ctx, *newMessage)
}

func (t *Transport) cleanFilters() error {
	return t.filters.RemoveNoListenFilters()
}

func (t *Transport) addSig(newMessage *types.NewMessage) error {
	sigID, err := t.keysManager.AddOrGetKeyPair(t.keysManager.privateKey)
	if err != nil {
		return err
	}
	newMessage.SigID = sigID
	return nil
}

func (t *Transport) Track(identifiers [][]byte, hash []byte, newMessage *types.NewMessage) {
	if t.envelopesMonitor != nil {
		t.envelopesMonitor.Add(identifiers, types.BytesToHash(hash), *newMessage)
	}
}

// GetCurrentTime returns the current unix timestamp in milliseconds
func (t *Transport) GetCurrentTime() uint64 {
	return uint64(t.waku.GetCurrentTime().UnixNano() / int64(time.Millisecond))
}

func (t *Transport) MaxMessageSize() uint32 {
	return t.waku.MaxMessageSize()
}

func (t *Transport) Stop() error {
	close(t.quit)
	if t.envelopesMonitor != nil {
		t.envelopesMonitor.Stop()
	}
	return nil
}

// cleanFiltersLoop cleans up the topic we create for the only purpose
// of sending messages.
// Whenever we send a message we also need to listen to that particular topic
// but in case of asymettric topics, we are not interested in listening to them.
// We therefore periodically clean them up so we don't receive unnecessary data.

func (t *Transport) cleanFiltersLoop() {

	ticker := time.NewTicker(5 * time.Minute)
	go func() {
		for {
			select {
			case <-t.quit:
				ticker.Stop()
				return
			case <-ticker.C:
				err := t.cleanFilters()
				if err != nil {
					t.logger.Error("failed to clean up topics", zap.Error(err))
				}
			}
		}
	}()
}

func (t *Transport) sendMessagesRequestForTopics(
	ctx context.Context,
	peerID []byte,
	from, to uint32,
	previousCursor []byte,
	topics []types.TopicType,
	waitForResponse bool,
) (cursor []byte, err error) {

	r := createMessagesRequest(from, to, previousCursor, topics)
	r.SetDefaults(t.waku.GetCurrentTime())

	events := make(chan types.EnvelopeEvent, 10)
	sub := t.waku.SubscribeEnvelopeEvents(events)
	defer sub.Unsubscribe()

	err = t.waku.SendMessagesRequest(peerID, r)
	if err != nil {
		return
	}

	if !waitForResponse {
		return
	}

	resp, err := t.waitForRequestCompleted(ctx, r.ID, events)
	if err == nil && resp != nil && resp.Error != nil {
		err = resp.Error
	} else if err == nil && resp != nil {
		cursor = resp.Cursor
	}
	return
}

// RequestHistoricMessages requests historic messages for all registered filters.
func (t *Transport) SendMessagesRequest(
	ctx context.Context,
	peerID []byte,
	from, to uint32,
	previousCursor []byte,
	waitForResponse bool,
) (cursor []byte, err error) {

	topics := make([]types.TopicType, len(t.Filters()))
	for _, f := range t.Filters() {
		topics = append(topics, f.Topic)
	}

	return t.sendMessagesRequestForTopics(ctx, peerID, from, to, previousCursor, topics, waitForResponse)
}

func (t *Transport) SendMessagesRequestForFilter(
	ctx context.Context,
	peerID []byte,
	from, to uint32,
	previousCursor []byte,
	filter *Filter,
	waitForResponse bool,
) (cursor []byte, err error) {

	topics := make([]types.TopicType, len(t.Filters()))
	topics = append(topics, filter.Topic)

	return t.sendMessagesRequestForTopics(ctx, peerID, from, to, previousCursor, topics, waitForResponse)
}

func createMessagesRequest(from, to uint32, cursor []byte, topics []types.TopicType) types.MessagesRequest {
	aUUID := uuid.New()
	// uuid is 16 bytes, converted to hex it's 32 bytes as expected by types.MessagesRequest
	id := []byte(hex.EncodeToString(aUUID[:]))
	return types.MessagesRequest{
		ID:     id,
		From:   from,
		To:     to,
		Limit:  100,
		Cursor: cursor,
		Bloom:  topicsToBloom(topics...),
	}
}

func topicsToBloom(topics ...types.TopicType) []byte {
	i := new(big.Int)
	for _, topic := range topics {
		bloom := types.TopicToBloom(topic)
		i.Or(i, new(big.Int).SetBytes(bloom[:]))
	}

	combined := make([]byte, types.BloomFilterSize)
	data := i.Bytes()
	copy(combined[types.BloomFilterSize-len(data):], data[:])

	return combined
}

func (t *Transport) waitForRequestCompleted(ctx context.Context, requestID []byte, events chan types.EnvelopeEvent) (*types.MailServerResponse, error) {
	for {
		select {
		case ev := <-events:
			if !bytes.Equal(ev.Hash.Bytes(), requestID) {
				continue
			}
			if ev.Event != types.EventMailServerRequestCompleted {
				continue
			}
			data, ok := ev.Data.(*types.MailServerResponse)
			if ok {
				return data, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// ConfirmMessagesProcessed marks the messages as processed in the cache so
// they won't be passed to the next layer anymore
func (t *Transport) ConfirmMessagesProcessed(ids []string, timestamp uint64) error {
	return t.cache.Add(ids, timestamp)
}

// CleanMessagesProcessed clears the messages that are older than timestamp
func (t *Transport) CleanMessagesProcessed(timestamp uint64) error {
	return t.cache.Clean(timestamp)
}

func (t *Transport) SetEnvelopeEventsHandler(handler EnvelopeEventsHandler) error {
	if t.envelopesMonitor == nil {
		return errors.New("Current transport has no envelopes monitor")
	}
	t.envelopesMonitor.handler = handler
	return nil
}

func PubkeyToHex(key *ecdsa.PublicKey) string {
	return types.EncodeHex(crypto.FromECDSAPub(key))
}
