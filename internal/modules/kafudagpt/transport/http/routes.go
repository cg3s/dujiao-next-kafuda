package http

import "github.com/gin-gonic/gin"

func RegisterAdminRoutes(admin gin.IRoutes, handler *AdminHandler) {
	admin.GET("/kafuda-gpt/catalog", handler.GetCatalog)
	admin.POST("/kafuda-gpt/catalog/sync", handler.SyncCatalog)
}
