package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"

	"github.com/stretchr/testify/assert"
)

func TestPiRouteHeaderAllowsOnlyVersionedProtocolPaths(t *testing.T) {
	for _, tc := range []struct {
		capability, path string
		allowed          bool
	}{
		{"model-routes-v2", "/v1/chat/completions", true},
		{"model-routes-v1,model-routes-v2", "/v1/responses", true},
		{"model-routes-v2", "/v1/responses/compact", true},
		{"model-routes-v1", "/v1/chat/completions", false},
		{"model-routes-v2", "/v1/messages", false},
		{"xmodel-routes-v2", "/v1/chat/completions", false},
	} {
		t.Run(tc.capability+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			req.Header.Set("X-Twork-Channel-Id", "168")
			req.Header.Set("X-Twork-Client-Version", "4.0.0")
			req.Header.Set("X-Twork-Client-Capabilities", tc.capability)
			id, present, err := parseTworkChannelRoute(req)
			assert.True(t, present)
			if tc.allowed {
				assert.NoError(t, err)
				assert.Equal(t, 168, id)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestPiDistributeChecksActualChannelProtocolAndAuthorization(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, tc := range []struct {
		wire, path, capability string
		grant                  bool
		want                   int
	}{
		{"responses", "/v1/responses", "model-routes-v2", true, 200},
		{"responses", "/v1/responses/compact", "model-routes-v2", true, 200},
		{"chat_completions", "/v1/chat/completions", "model-routes-v2", true, 200},
		{"responses", "/v1/chat/completions", "model-routes-v2", true, 403},
		{"chat_completions", "/v1/responses", "model-routes-v2", true, 403},
		{"chat_completions", "/v1/responses/compact", "model-routes-v2", true, 403},
		{"responses", "/v1/responses", "model-routes-v1", true, 403},
		{"responses", "/v1/responses", "model-routes-v2", false, 403},
	} {
		t.Run(tc.wire+tc.path+tc.capability, func(t *testing.T) {
			setupDistributorTokenAffinityDB(t)
			seedDistributorChannel(t, 4701, "gpt-5", 100)
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4701).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"`+tc.wire+`"}`).Error)
			if tc.grant {
				require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 995, ModelId: "gpt-5", ChannelId: 4701}).Error)
			}
			result := requestRuntimeRoute(t, 995, "default", tc.path, `{"model":"gpt-5"}`, tc.want, func(c *gin.Context) {
				c.Request.Header.Set("X-Twork-Channel-Id", "4701")
				c.Request.Header.Set("X-Twork-Client-Version", "4.0.0")
				c.Request.Header.Set("X-Twork-Client-Capabilities", tc.capability)
				c.Set("role", 100)
			})
			if tc.want == 200 {
				assert.Equal(t, 4701, result.ChannelID)
			}
		})
	}
}
