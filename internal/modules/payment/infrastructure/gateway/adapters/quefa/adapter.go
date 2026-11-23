package quefaadapter

import (
	"context"
	"errors"
	"strings"

	"github.com/dujiao-next/internal/constants"
	paymentcontract "github.com/dujiao-next/internal/modules/payment/contract"
	gatewaycommon "github.com/dujiao-next/internal/modules/payment/infrastructure/gateway/common"
	"github.com/dujiao-next/internal/shared/jsonmap"
	"github.com/shopspring/decimal"
)

// PlatformCollectCreator bridges the payment provider adapter to the Quefa order service
// without introducing a direct dependency on Quefa's application package.
type PlatformCollectCreator func(ctx context.Context, parentOrderID uint, onlineAmount decimal.Decimal, returnURL string) (upstreamOrderID, payURL, qrCode string, err error)

type Adapter struct {
	create PlatformCollectCreator
}

var _ paymentcontract.GatewayProvider = (*Adapter)(nil)

func NewAdapter(create PlatformCollectCreator) *Adapter {
	return &Adapter{create: create}
}

func (a *Adapter) SetCreator(create PlatformCollectCreator) {
	if a != nil {
		a.create = create
	}
}

func (a *Adapter) Type() string {
	return constants.PaymentProviderQuefa + ":"
}

func (a *Adapter) ValidateConfig(raw jsonmap.JSON, _ string) error {
	if raw == nil {
		return nil
	}
	if rawReturn, ok := raw["return_url"].(string); ok {
		trimmed := strings.ToLower(strings.TrimSpace(rawReturn))
		if trimmed != "" {
			if !strings.HasPrefix(trimmed, "https://") && !strings.HasPrefix(trimmed, "http://") {
				return paymentcontract.ErrGatewayConfigInvalid
			}
			if strings.Contains(trimmed, "tibo.ink") || strings.Contains(trimmed, "quefa") {
				return paymentcontract.ErrGatewayConfigInvalid
			}
		}
	}
	return nil
}

func (a *Adapter) CreatePayment(ctx context.Context, raw jsonmap.JSON, input paymentcontract.GatewayCreateInput) (*paymentcontract.GatewayCreateResult, error) {
	if a == nil || a.create == nil {
		return nil, errors.New("quefa payment adapter not wired")
	}
	returnURL := strings.TrimSpace(input.ReturnURL)
	if returnURL == "" && raw != nil {
		if cfgReturn, ok := raw["return_url"].(string); ok {
			returnURL = strings.TrimSpace(cfgReturn)
		}
	}
	if returnURL != "" {
		returnURL = gatewaycommon.AppendQueryParams(returnURL, input.ReturnURLQuery)
	}
	upstreamID, payURL, qrCode, err := a.create(ctx, input.OrderID, input.Amount.Decimal.Round(2), returnURL)
	if err != nil {
		return nil, err
	}
	return &paymentcontract.GatewayCreateResult{
		ProviderRef:  upstreamID,
		RedirectURL:  payURL,
		QRCodeURL:    qrCode,
		AmountSent:   input.Amount.Decimal.Round(2).StringFixed(2),
		CurrencySent: strings.ToUpper(strings.TrimSpace(input.Currency)),
	}, nil
}
