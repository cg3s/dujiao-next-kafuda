package kafudawiring

import kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"

func NewAdminHandler(module *Module) *kafudahttp.AdminHandler {
	if module == nil {
		return &kafudahttp.AdminHandler{}
	}
	handler := &kafudahttp.AdminHandler{
		Catalog: module.Catalog, Configured: module.Gateway != nil, GatewayConfigInvalid: module.gatewayConfigInvalid,
	}
	if module.Commerce != nil {
		handler.SalesBlockReason = module.Commerce.SalesBlockReason
	}
	return handler
}
