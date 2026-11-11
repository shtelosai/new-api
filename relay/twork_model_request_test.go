package relay

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkModelRoutePreservesNativeGeminiConversion(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"public-gemini","messages":[{"role":"system","content":"policy"},{"role":"user","content":"hello"}],"max_completion_tokens":16}`))
	info := &relaycommon.RelayInfo{OriginModelName: "public-gemini", ChannelMeta: &relaycommon.ChannelMeta{
		ApiType: constant.APITypeGemini, ChannelType: constant.ChannelTypeGemini, UpstreamModelName: "gemini-3.8-flash-tiered",
		ChannelSetting: dto.ChannelSettings{TworkRuntime: "pi", TworkWireAPI: "chat_completions"},
	}}
	data, err := buildTworkModelUpstreamRequest(c, info, GetAdaptor(constant.APITypeGemini))
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	assert.NotContains(t, body, "messages")
	require.Contains(t, body, "contents")
	assert.Contains(t, string(data), "hello")
	assert.Contains(t, string(data), "policy")
}

func TestTworkModelRoutePreservesOpenAIAdaptorAndExtensions(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"public-gpt","input":"hi","custom_extension":{"enabled":true}}`))
	info := &relaycommon.RelayInfo{OriginModelName: "public-gpt", ChannelMeta: &relaycommon.ChannelMeta{ApiType: constant.APITypeOpenAI, ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-5-high", ChannelSetting: dto.ChannelSettings{TworkRuntime: "pi", TworkWireAPI: "responses"}}}
	data, err := buildTworkModelUpstreamRequest(c, info, GetAdaptor(constant.APITypeOpenAI))
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	assert.Equal(t, "gpt-5", body["model"])
	assert.Equal(t, map[string]any{"effort": "high"}, body["reasoning"])
	assert.Equal(t, map[string]any{"enabled": true}, body["custom_extension"])
}

// OMP Claude 模型级请求经 Anthropic 适配器出站时，历史思考既不能带签名回放，也不能改写成正文（Twork 飞书 #76）。
func TestTworkModelRouteOMPAnthropicSendsNoHistoryThinking(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-opus-5-5","max_tokens":128,"stream":true,
		"thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},
		"tools":[{"name":"bash","description":"run","input_schema":{"type":"object","properties":{}}}],
		"messages":[
			{"role":"user","content":"帮我统一一下颜色"},
			{"role":"assistant","content":[{"type":"thinking","thinking":"I'm defining a teal ramp next.","signature":"sig-a"},{"type":"tool_use","id":"call-1","name":"bash","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"ok"}]}
		]}`))
	common.SetContextKey(c, constant.ContextKeyTworkOMPVersion, "18.12.2")
	t.Cleanup(func() { common.CleanupBodyStorage(c) })
	info := &relaycommon.RelayInfo{OriginModelName: "claude-opus-5-5", ChannelMeta: &relaycommon.ChannelMeta{
		ApiType: constant.APITypeAnthropic, ChannelType: constant.ChannelTypeAnthropic, UpstreamModelName: "claude-opus-5-5",
		ChannelSetting: dto.ChannelSettings{TworkRuntime: "omp", TworkWireAPI: "anthropic-messages"},
	}}
	data, err := buildTworkModelUpstreamRequest(c, info, GetAdaptor(constant.APITypeAnthropic))
	require.NoError(t, err)
	assert.NotContains(t, string(data), "teal ramp")
	assert.NotContains(t, string(data), "sig-a")
	var body struct {
		Thinking map[string]any `json:"thinking"`
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, common.Unmarshal(data, &body))
	// 本次请求的思考参数不受影响，只是历史里不回放。
	assert.Equal(t, "adaptive", body.Thinking["type"])
	require.Len(t, body.Messages, 3)
	assert.Equal(t, []any{map[string]any{"type": "tool_use", "id": "call-1", "name": "bash", "input": map[string]any{}}}, body.Messages[1].Content)
	assert.Equal(t, "call-1", body.Messages[2].Content.([]any)[0].(map[string]any)["tool_use_id"])
}
