package kafuda

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultFXCNYURL = "https://open.er-api.com/v6/latest/CNY"
	fxCacheTTL      = time.Hour
	fxStaleMaxAge   = 24 * time.Hour
	fxHTTPTimeout   = 3 * time.Second
	fxMaxBodyBytes  = 512 * 1024
)

// FXClient fetches and caches live foreign-currency-to-CNY exchange rates.
type FXClient struct {
	endpoint string
	client   *http.Client

	mu        sync.Mutex
	rates     map[string]float64
	updatedAt time.Time
	fetchedAt time.Time
}

// NewFXClient constructs a cached CNY exchange-rate provider.
// When endpoint or client is empty/nil, sensible production defaults are used.
func NewFXClient(endpoint string, client *http.Client) *FXClient {
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultFXCNYURL
	}
	if client == nil {
		client = &http.Client{Timeout: fxHTTPTimeout}
	}
	return &FXClient{
		endpoint: endpoint,
		client:   client,
	}
}

// RatesToCNY returns a map where each entry is CNY per 1 unit of the keyed ISO-4217 currency.
func (f *FXClient) RatesToCNY(ctx context.Context) (map[string]float64, time.Time, error) {
	if f == nil {
		return nil, time.Time{}, errors.New("fx client not initialized")
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	now := time.Now().UTC()
	if len(f.rates) > 0 && now.Sub(f.fetchedAt) < fxCacheTTL {
		return cloneRates(f.rates), f.updatedAt, nil
	}

	rates, updatedAt, err := f.fetch(ctx)
	if err != nil {
		if len(f.rates) > 0 && now.Sub(f.fetchedAt) < fxStaleMaxAge {
			return cloneRates(f.rates), f.updatedAt, nil
		}
		return nil, time.Time{}, err
	}
	f.rates = rates
	f.updatedAt = updatedAt
	f.fetchedAt = now
	return cloneRates(rates), updatedAt, nil
}

func (f *FXClient) fetch(ctx context.Context) (map[string]float64, time.Time, error) {
	reqCtx, cancel := context.WithTimeout(ctx, fxHTTPTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, f.endpoint, nil)
	if err != nil {
		return nil, time.Time{}, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, time.Time{}, errors.New("fx upstream returned non-200 status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fxMaxBodyBytes+1))
	if err != nil || len(body) > fxMaxBodyBytes {
		return nil, time.Time{}, errors.New("fx response unreadable")
	}
	return parseFXCNYResponse(body)
}

func parseFXCNYResponse(body []byte) (map[string]float64, time.Time, error) {
	var wire struct {
		Result             string             `json:"result"`
		BaseCode           string             `json:"base_code"`
		TimeLastUpdateUnix int64              `json:"time_last_update_unix"`
		Rates              map[string]float64 `json:"rates"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return nil, time.Time{}, err
	}
	if strings.ToLower(strings.TrimSpace(wire.Result)) != "success" ||
		strings.ToUpper(strings.TrimSpace(wire.BaseCode)) != "CNY" ||
		len(wire.Rates) == 0 {
		return nil, time.Time{}, errors.New("fx response invalid")
	}
	toCNY := make(map[string]float64, len(wire.Rates)+1)
	toCNY["CNY"] = 1.0
	for code, perCNY := range wire.Rates {
		upper := strings.ToUpper(strings.TrimSpace(code))
		if upper == "" || perCNY <= 0 || math.IsNaN(perCNY) || math.IsInf(perCNY, 0) {
			continue
		}
		if upper == "CNY" {
			toCNY[upper] = 1.0
			continue
		}
		toCNY[upper] = 1.0 / perCNY
	}
	updatedAt := time.Now().UTC()
	if wire.TimeLastUpdateUnix > 0 {
		updatedAt = time.Unix(wire.TimeLastUpdateUnix, 0).UTC()
	}
	return toCNY, updatedAt, nil
}

func cloneRates(src map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
