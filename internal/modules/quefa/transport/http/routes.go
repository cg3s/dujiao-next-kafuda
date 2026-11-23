package http

import "github.com/gin-gonic/gin"

func RegisterAdminRoutes(admin gin.IRoutes, handler *AdminHandler) {
	admin.GET("/quefa/config", handler.GetConfig)
	admin.PUT("/quefa/config", handler.UpdateConfig)
	admin.POST("/quefa/config/test", handler.TestConfig)
	admin.GET("/quefa/catalog", handler.GetCatalog)
	admin.POST("/quefa/catalog/sync", handler.SyncCatalog)
	admin.GET("/orders/:id/quefa", handler.GetOrderStatus)
	admin.POST("/orders/:id/quefa/sync", handler.SyncOrder)
	admin.POST("/orders/:id/quefa/mark-review", handler.MarkOrderReview)
}

func RegisterWebhookRoutes(api gin.IRoutes, handler *WebhookHandler) {
	api.POST("/webhooks/quefa", handler.Handle)
}
