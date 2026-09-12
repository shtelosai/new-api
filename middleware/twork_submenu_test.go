package middleware

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiSubmenuRequiresExactAuthorizedChannel(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, tc := range []struct {
		name, wire, path     string
		granted, adminSuffix bool
		want                 int
	}{
		{"Responses授权", "responses", "/v1/responses", true, false, 200},
		{"Compact授权", "responses", "/v1/responses/compact", true, false, 200},
		{"Chat授权", "chat_completions", "/v1/chat/completions", true, false, 200},
		{"管理员无授权", "responses", "/v1/responses", false, false, 403},
		{"协议不符", "responses", "/v1/chat/completions", true, false, 403},
		{"管理员后缀不能代替显式请求", "responses", "/v1/responses", true, true, 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDistributorTokenAffinityDB(t)
			seedDistributorChannel(t, 4801, "gpt-5", 100)
			seedDistributorChannel(t, 4802, "gpt-5", 0)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4801).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"`+tc.wire+`","twork_display_mode":"submenu"}`).Error)
			// 即使另一个同名渠道有授权，固定渠道无授权也不能回退。
			require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 998, ModelId: "gpt-5", ChannelId: 4802}).Error)
			if tc.granted {
				require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 998, ModelId: "gpt-5", ChannelId: 4801}).Error)
			}
			model.InitChannelCache()
			result := requestRuntimeRoute(t, 998, "default", tc.path, `{"model":"gpt-5"}`, tc.want, func(c *gin.Context) {
				c.Set("role", 100)
				if tc.adminSuffix {
					common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "4801")
				} else {
					c.Request.Header.Set("X-Twork-Channel-Id", "4801")
					c.Request.Header.Set("X-Twork-Client-Version", "4.0.0")
					c.Request.Header.Set("X-Twork-Client-Capabilities", "model-routes-v2")
				}
			})
			if tc.want == http.StatusOK {
				assert.Equal(t, 4801, result.ChannelID)
			}
		})
	}
}
