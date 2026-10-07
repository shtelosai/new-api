package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func SetImageJobRouter(engine *gin.Engine) {
	r := engine.Group("/v1/image-tasks", middleware.RouteTag("image_tasks"), middleware.ImageJobAuth())
	r.GET("/capabilities", controller.ImageJobCapabilities)
	r.POST("/accounting/prepare", controller.PrepareImageJobAccounting)
	r.POST("", controller.CreateImageJob)
	r.GET("", controller.ListImageJobs)
	r.GET("/:id", controller.GetImageJob)
	r.GET("/:id/result", controller.ImageJobResult)
}
