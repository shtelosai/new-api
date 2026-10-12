package common

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOMPUsesNativeThinkingSnapshotAndKeepsWireVersionSeparate(t *testing.T) {
	for _, tc := range []struct{ wire, path, profile, level string }{
		{"chat-completions", "/v1/chat/completions", "bailian", "off"},
		{"chat-completions", "/v1/chat/completions", "bailian", "high"},
		{"chat-completions", "/v1/chat/completions", "", "unsupported"},
		{"responses", "/v1/responses", "openai", "high"},
	} {
		t.Run(tc.wire+tc.profile+tc.level, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"original","max_completion_tokens":16,"reasoning_effort":"high","reasoning":{"effort":"high"},"messages":[{"role":"developer","content":"policy"}],"input":[]}`))
			c.Request.Header.Set("X-Twork-Thinking-Level", tc.level)
			common.SetContextKey(c, constant.ContextKeyTworkOMPVersion, "18.10.1")
			info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "unknown-alias", ChannelSetting: dto.ChannelSettings{TworkRuntime: "omp", TworkWireAPI: tc.wire, TworkOMPCompatibility: tc.profile}}}
			data, err := BuildTworkModelRequest(c, info)
			require.NoError(t, err)
			var body map[string]any
			require.NoError(t, common.Unmarshal(data, &body))
			assert.Equal(t, "unknown-alias", body["model"])
			if tc.level == "unsupported" {
				assert.NotContains(t, body, "reasoning_effort")
				assert.NotContains(t, body, "reasoning")
			} else if tc.profile == "bailian" {
				assert.Equal(t, tc.level != "off", body["enable_thinking"])
				assert.NotContains(t, body, "reasoning_effort")
			} else {
				assert.Equal(t, "high", body["reasoning"].(map[string]any)["effort"])
			}
		})
	}
}

func TestOMPAnthropicDropsOnlyChannelBoundThinkingAndPreservesToolPairs(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alias","messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reason","signature":"old-channel"},{"type":"tool_use","id":"call-1","name":"lookup","input":{}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"result"}]}]}`))
	common.SetContextKey(c, constant.ContextKeyTworkOMPVersion, "18.10.1")
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "mapped"}})
	require.NoError(t, err)
	assert.NotContains(t, string(data), "old-channel")
	assert.Contains(t, string(data), "reason")
	assert.Contains(t, string(data), `"tool_use_id":"call-1"`)
	assert.Contains(t, string(data), `"type":"tool_use"`)
}
