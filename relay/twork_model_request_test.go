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
