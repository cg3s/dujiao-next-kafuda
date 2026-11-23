package domain

import (
	"errors"
	"net/url"
	"strings"
)

var (
	ErrPaymentDomainForbidden  = errors.New("payment domain is forbidden (contains upstream brand)")
	ErrPaymentDomainNotAllowed = errors.New("payment domain is not in allowed whitelist")
	ErrPaymentURLInvalid       = errors.New("payment url must be a valid https url")
	ErrFulfillmentURLInvalid   = errors.New("fulfillment url must be a valid https url")
)

// IsForbiddenPaymentHost reports whether host belongs to Quefa's default domain or brand.
func IsForbiddenPaymentHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" {
		return true
	}
	if h == "tibo.ink" || strings.HasSuffix(h, ".tibo.ink") {
		return true
	}
	if strings.Contains(h, "quefa") {
		return true
	}
	return false
}

// MatchesAllowedDomain checks whether host matches any configured exact or wildcard (*.example.com) domain.
func MatchesAllowedDomain(host string, allowedDomains []string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || len(allowedDomains) == 0 {
		return false
	}
	for _, raw := range allowedDomains {
		pattern := strings.ToLower(strings.TrimSpace(raw))
		if pattern == "" {
			continue
		}
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:] // ".example.com"
			if strings.HasSuffix(h, suffix) && len(h) > len(suffix) {
				return true
			}
			continue
		}
		if h == pattern {
			return true
		}
	}
	return false
}

// ValidateAllowedPayDomains validates admin-configured allowed payment domain patterns.
func ValidateAllowedPayDomains(domains []string) ([]string, error) {
	seen := make(map[string]struct{}, len(domains))
	out := make([]string, 0, len(domains))
	for _, raw := range domains {
		d := strings.ToLower(strings.TrimSpace(raw))
		if d == "" {
			continue
		}
		if strings.ContainsAny(d, " /\\:@?#\r\n") {
			return nil, ErrPaymentDomainNotAllowed
		}
		probe := strings.TrimPrefix(d, "*.")
		if probe == "" || !strings.Contains(probe, ".") || IsForbiddenPaymentHost(probe) {
			return nil, ErrPaymentDomainForbidden
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out, nil
}

// ValidatePaymentEntryURL verifies that a buyer-facing payment URL uses HTTPS,
// never points to Quefa / tibo.ink, and matches the configured allowlist.
func ValidatePaymentEntryURL(rawURL string, allowedDomains []string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ErrPaymentURLInvalid
	}
	u, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.User != nil {
		return ErrPaymentURLInvalid
	}
	host := strings.ToLower(strings.TrimSpace(u.Hostname()))
	if host == "" {
		return ErrPaymentURLInvalid
	}
	if IsForbiddenPaymentHost(host) {
		return ErrPaymentDomainForbidden
	}
	if !MatchesAllowedDomain(host, allowedDomains) {
		return ErrPaymentDomainNotAllowed
	}
	return nil
}

// ValidatePaymentQRCode checks a payment QR payload. If it encodes an HTTP(S) URL,
// it enforces the same white-label domain policy; non-URL payment schemes (e.g. weixin://,
// alipays://, EMVCo QR payloads) are still screened for forbidden Quefa domains.
func ValidatePaymentQRCode(qrContent string, allowedDomains []string) error {
	trimmed := strings.TrimSpace(qrContent)
	if trimmed == "" {
		return ErrPaymentURLInvalid
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "tibo.ink") || strings.Contains(lower, "quefa") {
		return ErrPaymentDomainForbidden
	}
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return ValidatePaymentEntryURL(trimmed, allowedDomains)
	}
	return nil
}

// ValidateFulfillmentURL verifies that a hosted recharge entry URL is a safe HTTPS URL.
func ValidateFulfillmentURL(rawURL string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ErrFulfillmentURLInvalid
	}
	u, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" || u.User != nil {
		return ErrFulfillmentURLInvalid
	}
	return nil
}
