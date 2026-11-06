package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/gin-gonic/gin"
)

func SetImageJobRouter(engine *gin.Engine) {
	r := engine.Group("/v1/image-tasks", middleware.RouteTag("image_tasks"), middleware.ImageJobAuth(), middleware.CaptureTworkUsage)
	r.GET("/capabilities", controller.ImageJobCapabilities)
	r.POST("/accounting/prepare", controller.PrepareImageJobAccounting)
	r.POST("", controller.CreateImageJob)
	r.GET("", controller.ListImageJobs)
	r.GET("/:id", controller.GetImageJob)
	r.GET("/:id/result", controller.ImageJobResult)
	// 画布独立路由：返回体带实际模型和价格，旧接口的返回内容保持不变。
	canvas := engine.Group("/v1/canvas-image-tasks", middleware.RouteTag("image_tasks"), middleware.ImageJobAuth())
	canvas.GET("/models", controller.ListImageModels)
	canvas.POST("", controller.CreateCanvasImageJob)
	canvas.GET("/:id", controller.GetCanvasImageJob)
	canvas.GET("/:id/result", controller.ImageJobResult)
}
