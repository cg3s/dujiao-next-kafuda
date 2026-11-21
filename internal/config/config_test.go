package config

import (
	"testing"
	"time"
)

func TestKafudaCatalogRefreshInterval(t *testing.T) {
	tests := []struct {
		name      string
		cfg       *Config
		want      time.Duration
		wantFixed bool
	}{
		{
			name:      "nil config",
			cfg:       nil,
			want:      0,
			wantFixed: false,
		},
		{
			name: "zero ttl disables refresh",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				CatalogTTLSeconds:             0,
				CatalogRefreshIntervalSeconds: 60,
			}},
			want:      0,
			wantFixed: false,
		},
		{
			name: "negative refresh interval disables refresh",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				SalesEnabled:                  true,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: -1,
			}},
			want:      0,
			wantFixed: false,
		},
		{
			name: "auto mode works even when sales_enabled is false",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 0,
			}},
			want:      time.Minute,
			wantFixed: false,
		},
		{
			name: "auto mode scales down for short ttl",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             24,
				CatalogRefreshIntervalSeconds: 0,
			}},
			want:      6 * time.Second,
			wantFixed: false,
		},
		{
			name: "fixed interval mode works without sales_enabled",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				SalesEnabled:                  false,
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 300,
			}},
			want:      5 * time.Minute,
			wantFixed: true,
		},
		{
			name: "fixed interval clamps minimum to 5s",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 2,
			}},
			want:      5 * time.Second,
			wantFixed: true,
		},
		{
			name: "fixed interval clamps maximum to 24h",
			cfg: &Config{KafudaGPT: KafudaGPTConfig{
				CatalogTTLSeconds:             3600,
				CatalogRefreshIntervalSeconds: 100000,
			}},
			want:      24 * time.Hour,
			wantFixed: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.KafudaCatalogRefreshInterval(); got != tc.want {
				t.Fatalf("KafudaCatalogRefreshInterval() = %v, want %v", got, tc.want)
			}
			if got := tc.cfg.KafudaCatalogFixedRefresh(); got != tc.wantFixed {
				t.Fatalf("KafudaCatalogFixedRefresh() = %v, want %v", got, tc.wantFixed)
			}
		})
	}
}
