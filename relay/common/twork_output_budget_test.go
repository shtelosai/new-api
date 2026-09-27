package common

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkModelRouteOutputBudgetUsesExactProviderModel(t *testing.T) {
	for _, tc := range []struct{ profile, model, field string }{
		{"bailian", "ZHIPU/GLM-5.3-Flash", "max_tokens"},
		{"bailian", "glm-5.3-flash", "max_tokens"},
		{"china-telecom", "glm-5.3-flash", "max_tokens"},
		{"bailian", "kimi-k2.6", "max_completion_tokens"},
		{"china-telecom", "qwen3.8-flash", "max_completion_tokens"},
		{"bailian", "glm-unverified-alias", "max_completion_tokens"},
		{"", "ZHIPU/GLM-5.3-Flash", "max_completion_tokens"},
	} {
		t.Run(tc.profile+"/"+tc.model, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"public-model","max_completion_tokens":16,"messages":[{"role":"user","content":"验收"}]}`))
			data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: tc.model,
				ChannelSetting: dto.ChannelSettings{TworkWireAPI: "chat_completions", TworkPiCompatibility: tc.profile}}})
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.Unmarshal(data, &body))
			assert.Equal(t, float64(16), body[tc.field])
			other := "max_tokens"
			if tc.field == other {
				other = "max_completion_tokens"
			}
			assert.NotContains(t, body, other)
			assert.Equal(t, tc.model, body["model"])
		})
	}
}
