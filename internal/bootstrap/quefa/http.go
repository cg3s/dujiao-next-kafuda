package quefawiring

import (
	quefaclient "github.com/dujiao-next/internal/modules/quefa/infrastructure/client"
	quefahttp "github.com/dujiao-next/internal/modules/quefa/transport/http"
)

func NewAdminHandler(mod *Module) *quefahttp.AdminHandler {
	if mod == nil {
		return &quefahttp.AdminHandler{}
	}
	return &quefahttp.AdminHandler{
		Config:  mod.Config,
		Catalog: mod.Catalog,
		Orders:  mod.Orders,
	}
}

func NewWebhookHandler(mod *Module) *quefahttp.WebhookHandler {
	if mod == nil {
		return &quefahttp.WebhookHandler{}
	}
	return &quefahttp.WebhookHandler{
		Config:   mod.Config,
		Verifier: quefaclient.VerifyWebhookSignature,
		Ingestor: mod.Orders,
	}
}
