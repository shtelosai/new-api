package middleware

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
)

// CaptureTworkUsage also covers multipart and asynchronous task entry points.
func CaptureTworkUsage(c *gin.Context) {
	c.Set(common.TworkUsageContextKey, common.ParseTworkUsageHeaders(c.Request.Header))
}
