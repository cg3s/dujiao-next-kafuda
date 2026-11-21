package kafudawiring

import kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"

func NewAdminHandler(module *Module) *kafudahttp.AdminHandler {
	if module == nil {
		return &kafudahttp.AdminHandler{}
	}
	configured, invalid := module.GatewayStatus()
	handler := &kafudahttp.AdminHandler{
		Catalog:              module.Catalog,
		Config:               module.Config,
		FXRates:              module.FXRates,
		Configured:           configured,
		GatewayConfigInvalid: invalid,
		IsConfigured: func() bool {
			ok, _ := module.GatewayStatus()
			return ok
		},
		IsGatewayConfigInvalid: func() bool {
			_, bad := module.GatewayStatus()
			return bad
		},
		WorkerReady: module.WorkerReady,
	}
	if module.Commerce != nil {
		handler.SalesBlockReason = module.Commerce.CatalogSalesBlockReason
	}
	return handler
}
