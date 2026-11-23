package application

import (
	"github.com/dujiao-next/internal/constants"
	paymentdomain "github.com/dujiao-next/internal/modules/payment/domain"
)

// SetQuefaIntegration 注入 Quefa 托管直充收款模式与支付后采购回调钩子。
func (s *PaymentService) SetQuefaIntegration(collectionMode func() string, onOrderPaid func(orderID uint)) {
	s.quefaCollectionMode = collectionMode
	s.quefaPaidHook = onOrderPaid
}

func filterOutQuefaChannels(channels []paymentdomain.PaymentChannel) []paymentdomain.PaymentChannel {
	out := make([]paymentdomain.PaymentChannel, 0, len(channels))
	for _, ch := range channels {
		if ch.ProviderType == constants.PaymentProviderQuefa {
			continue
		}
		out = append(out, ch)
	}
	return out
}

func filterOnlyQuefaChannels(channels []paymentdomain.PaymentChannel) []paymentdomain.PaymentChannel {
	out := make([]paymentdomain.PaymentChannel, 0, len(channels))
	for _, ch := range channels {
		if ch.ProviderType == constants.PaymentProviderQuefa {
			out = append(out, ch)
		}
	}
	return out
}
