package protocol

import (
	"database/sql"
	"github.com/status-im/status-go/protocol/common"
	"github.com/status-im/status-go/protocol/encryption"
	"github.com/status-im/status-go/protocol/protobuf"
	"github.com/status-im/status-go/protocol/push_notification_server"
	"github.com/status-im/status-go/protocol/transport"
	"go.uber.org/zap"
)

type config struct {
	// This needs to be exposed until we move here mailserver logic
	// as otherwise the client is not notified of a new filter and
	// won't be pulling messages from mailservers until it reloads the chats/filters
	onNegotiatedFilters func([]*transport.Filter)
	// DEPRECATED: no need to expose it
	onSendContactCodeHandler func(*encryption.ProtocolMessageSpec)

	// systemMessagesTranslations holds translations for system-messages
	systemMessagesTranslations map[protobuf.MembershipUpdateEvent_EventType]string
	// Config for the envelopes monitor
	envelopesMonitorConfig *transport.EnvelopesMonitorConfig

	messagesPersistenceEnabled bool
	featureFlags               common.FeatureFlags

	// A path to a database or a database instance is required.
	// The database instance has a higher priority.
	dbConfig dbConfig
	db       *sql.DB

	verifyTransactionClient EthClient

	pushNotificationServerConfig *push_notification_server.Config

	logger *zap.Logger
}

type Option func(*config) error

// WithSystemMessagesTranslations is required for Group Chats which are currently disabled.
// nolint: unused
func WithSystemMessagesTranslations(t map[protobuf.MembershipUpdateEvent_EventType]string) Option {
	return func(c *config) error {
		c.systemMessagesTranslations = t
		return nil
	}
}

func WithOnNegotiatedFilters(h func([]*transport.Filter)) Option {
	return func(c *config) error {
		c.onNegotiatedFilters = h
		return nil
	}
}

func WithCustomLogger(logger *zap.Logger) Option {
	return func(c *config) error {
		c.logger = logger
		return nil
	}
}

func WithMessagesPersistenceEnabled() Option {
	return func(c *config) error {
		c.messagesPersistenceEnabled = true
		return nil
	}
}

func WithDatabaseConfig(dbPath, dbKey string) Option {
	return func(c *config) error {
		c.dbConfig = dbConfig{dbPath: dbPath, dbKey: dbKey}
		return nil
	}
}

func WithVerifyTransactionClient(client EthClient) Option {
	return func(c *config) error {
		c.verifyTransactionClient = client
		return nil
	}
}

func WithDatabase(db *sql.DB) Option {
	return func(c *config) error {
		c.db = db
		return nil
	}
}

func WithPushNotificationServerConfig(pushNotificationServerConfig *push_notification_server.Config) Option {
	return func(c *config) error {
		c.pushNotificationServerConfig = pushNotificationServerConfig
		return nil
	}
}

func WithDatasync() func(c *config) error {
	return func(c *config) error {
		c.featureFlags.Datasync = true
		return nil
	}
}

func WithEnvelopesMonitorConfig(emc *transport.EnvelopesMonitorConfig) Option {
	return func(c *config) error {
		c.envelopesMonitorConfig = emc
		return nil
	}
}
