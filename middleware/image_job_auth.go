package middleware

import (
	"net"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
)

const ImageJobTokenKey = "image_job_token_id"

// ImageJobAuth 每次直查 token，允许零余额领取已支付结果，不继承管理员共享 user_id 权限。
func ImageJobAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		parts := strings.Fields(header)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			imageJobAuthError(c, http.StatusUnauthorized, "image_auth_invalid")
			return
		}
		key := strings.TrimPrefix(parts[1], "sk-")
		token, user, err := model.ImageJobIdentity(c.Request.Context(), key)
		if err != nil {
			imageJobAuthError(c, http.StatusUnauthorized, "image_auth_invalid")
			return
		}
		if limits := token.GetIpLimits(); len(limits) > 0 {
			ip := net.ParseIP(c.ClientIP())
			if ip == nil || !common.IsIpInCIDRList(ip, limits) {
				imageJobAuthError(c, http.StatusForbidden, "image_access_denied")
				return
			}
		}
		group := token.Group
		if group != "" {
			if _, ok := service.GetUserUsableGroups(user.Group)[group]; !ok || group == "auto" || !ratio_setting.ContainsGroupRatio(group) {
				imageJobAuthError(c, http.StatusForbidden, "image_access_denied")
				return
			}
		}
		c.Set(ImageJobTokenKey, token.Id)
		c.Next()
	}
}
func imageJobAuthError(c *gin.Context, status int, code string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"code": code, "message": "图片任务访问凭据无效或无权限"}})
}
