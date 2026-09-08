package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkModelRouteRetriesWithIndependentUpstreamRequests(t *testing.T) {
	const name = "kimi-route-test"
	channels := setupRelaySoftCooldownChannels(t, name, 2, 2, 3)
	require.NoError(t, model.DB.AutoMigrate(&model.TokenModelChannel{}))
	for i, channel := range channels {
		profile := ""
		if i == 0 {
			profile = `,"twork_pi_compatibility":"bailian"`
		}
		require.NoError(t, model.DB.Model(channel).Updates(map[string]any{
			"type":          constant.ChannelTypeOpenAI,
			"setting":       `{"twork_runtime":"pi","twork_wire_api":"chat_completions","pass_through_body_enabled":true` + profile + `}`,
			"model_mapping": `{"kimi-route-test":"upstream-` + channel.Name + `"}`,
		}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 8701, ModelId: name, ChannelId: channel.Id}).Error)
	}
	model.InitChannelCache()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	original := client.Transport
	t.Cleanup(func() { client.Transport = original })
	var requests []map[string]any
	client.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		data, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, common.Unmarshal(data, &body))
		requests = append(requests, body)
		status, output := 503, `{"error":{"message":"retry channel","type":"server_error"}}`
		if len(requests) == 2 {
			status, output = 200, `{"id":"test-completion","object":"chat.completion","model":"kimi-route-test","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(output)), Request: req}, nil
	})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, 8701)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	router.Use(middleware.Distribute())
	router.POST("/v1/chat/completions", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kimi-route-test","max_completion_tokens":16,"reasoning_effort":"high","messages":[{"role":"developer","content":"policy"},{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Twork-Client-Version", "4.0.0")
	req.Header.Set("X-Twork-Client-Capabilities", "model-routes-v3")
	req.Header.Set("X-Twork-Route-Mode", "model")
	req.Header.Set("X-Twork-Agent-Runtime", "pi")
	req.Header.Set("X-Twork-Wire-Api", "chat_completions")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.Len(t, requests, 2)
	assert.Equal(t, "upstream-channel-9700", requests[0]["model"])
	assert.Equal(t, true, requests[0]["enable_thinking"])
	assert.NotContains(t, requests[0], "reasoning_effort")
	assert.Equal(t, "system", requests[0]["messages"].([]any)[0].(map[string]any)["role"])
	assert.Equal(t, "upstream-channel-9701", requests[1]["model"])
	assert.Equal(t, "high", requests[1]["reasoning_effort"])
	assert.NotContains(t, requests[1], "enable_thinking")
	assert.Equal(t, "developer", requests[1]["messages"].([]any)[0].(map[string]any)["role"])
}
