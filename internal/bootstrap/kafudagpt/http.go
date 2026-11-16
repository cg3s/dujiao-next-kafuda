package kafudawiring

import kafudahttp "github.com/dujiao-next/internal/modules/kafudagpt/transport/http"

func NewAdminHandler(module *Module) *kafudahttp.AdminHandler {
	if module == nil {
		return &kafudahttp.AdminHandler{}
	}
	return &kafudahttp.AdminHandler{Catalog: module.Catalog, Policy: module.Commerce, Configured: module.Gateway != nil}
}
