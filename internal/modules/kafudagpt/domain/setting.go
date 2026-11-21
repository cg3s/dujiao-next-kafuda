package domain

import "time"

const DefaultProfile = "default"

// Setting persists admin-managed Kafuda GPT direct-recharge configuration.
// APIKeyEncrypted stores AES-256-GCM ciphertext derived from app.secret_key.
type Setting struct {
	Profile                       string    `gorm:"primaryKey;type:varchar(64)" json:"-"`
	APIBaseURL                    string    `gorm:"type:varchar(512);not null;default:''" json:"-"`
	APIKeyEncrypted               string    `gorm:"type:text;not null;default:''" json:"-"`
	ProductCode                   string    `gorm:"type:varchar(128);not null;default:''" json:"-"`
	MaxCostCents                  int64     `gorm:"not null;default:0" json:"-"`
	AllowedKinds                  []string  `gorm:"serializer:json;type:json;not null" json:"-"`
	SalesEnabled                  bool      `gorm:"not null;default:false" json:"-"`
	ReferenceLookupVerified       bool      `gorm:"not null;default:false" json:"-"`
	CatalogTTLSeconds             int       `gorm:"not null;default:3600" json:"-"`
	CatalogRefreshIntervalSeconds int       `gorm:"not null;default:0" json:"-"`
	PollReviewAfterSeconds        int       `gorm:"not null;default:86400" json:"-"`
	PollIntervalSeconds           int       `gorm:"not null;default:10" json:"-"`
	Version                       int64     `gorm:"not null;default:1" json:"-"`
	UpdatedAt                     time.Time `gorm:"not null" json:"-"`
}

func (Setting) TableName() string { return "kafuda_gpt_settings" }
