package domain

import "time"

const (
	DefaultProfile    = "default"
	DefaultAPIBaseURL = "https://tibo.ink"
	FulfillmentType   = "quefa_hosted"

	CollectionModePlatformCollect = "platform_collect"
	CollectionModeAgentCollect    = "agent_collect"

	DeliveryModeAutoRecharge = "auto_recharge"
)

// IsValidCollectionMode reports whether mode is a supported Quefa collection mode.
func IsValidCollectionMode(mode string) bool {
	return mode == CollectionModePlatformCollect || mode == CollectionModeAgentCollect
}

// Setting persists global single-connection configuration for Quefa hosted direct recharge.
// ClientSecretEncrypted and WebhookSecretEncrypted store AES-256-GCM ciphertexts derived from app.secret_key.
type Setting struct {
	Profile                string    `gorm:"primaryKey;type:varchar(64)" json:"-"`
	APIBaseURL             string    `gorm:"type:varchar(512);not null;default:''" json:"-"`
	PartnerID              string    `gorm:"type:varchar(128);not null;default:''" json:"-"`
	KeyID                  string    `gorm:"type:varchar(128);not null;default:''" json:"-"`
	ClientSecretEncrypted  string    `gorm:"type:text;not null;default:''" json:"-"`
	WebhookSecretEncrypted string    `gorm:"type:text;not null;default:''" json:"-"`
	CollectionMode         string    `gorm:"type:varchar(32);not null;default:'platform_collect'" json:"-"`
	SalesEnabled           bool      `gorm:"not null;default:false" json:"-"`
	AllowedPayDomains      []string  `gorm:"serializer:json;type:json;not null" json:"-"`
	CatalogTTLSeconds      int       `gorm:"not null;default:3600" json:"-"`
	PollIntervalSeconds    int       `gorm:"not null;default:10" json:"-"`
	PollReviewAfterSeconds int       `gorm:"not null;default:86400" json:"-"`
	WebhookMaxSkewSeconds  int       `gorm:"not null;default:300" json:"-"`
	Version                int64     `gorm:"not null;default:1" json:"-"`
	UpdatedAt              time.Time `gorm:"not null" json:"-"`
}

func (Setting) TableName() string { return "quefa_settings" }
