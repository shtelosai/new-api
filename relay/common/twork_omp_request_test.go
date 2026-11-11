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
	assert.NotContains(t, string(data), "reason")
	assert.Contains(t, string(data), `"tool_use_id":"call-1"`)
	assert.Contains(t, string(data), `"type":"tool_use"`)
}

// 历史思考不能改写成 text：模型会把英文思考当成自己说过的话，长任务里照样写进正文（Twork 飞书 #76）。
func TestOMPAnthropicDropsHistoryThinkingInsteadOfReplayingAsText(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"alias","messages":[
		{"role":"user","content":"统一颜色"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"I'm defining a teal ramp next.","signature":"sig-a"},{"type":"text","text":"先看变量"},{"type":"tool_use","id":"call-1","name":"grep","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"--s1"}]},
		{"role":"assistant","content":[{"type":"thinking","thinking":"I'm settling the dark-mode ramp now.","signature":"sig-b"},{"type":"redacted_thinking","data":"opaque"}]},
		{"role":"user","content":"继续"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"I'm running the validator.","signature":"sig-c"},{"type":"tool_use","id":"call-2","name":"bash","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-2","content":"ok"}]}
	]}`))
	common.SetContextKey(c, constant.ContextKeyTworkOMPVersion, "18.12.2")
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "claude-opus-5-5"}})
	require.NoError(t, err)
	for _, leaked := range []string{"teal ramp", "dark-mode ramp", "validator", "sig-", "opaque", `"thinking"`} {
		assert.NotContains(t, string(data), leaked)
	}
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	require.NoError(t, common.Unmarshal(data, &body))
	// 只剩思考的那条助手消息整条去掉，前后两条用户消息合并；其余正文与工具配对保持原序。
	require.Len(t, body.Messages, 5)
	assert.Equal(t, []string{"user", "assistant", "user", "assistant", "user"}, []string{
		body.Messages[0].Role, body.Messages[1].Role, body.Messages[2].Role, body.Messages[3].Role, body.Messages[4].Role,
	})
	assert.Equal(t, "统一颜色", body.Messages[0].Content)
	assert.Equal(t, []any{
		map[string]any{"type": "text", "text": "先看变量"},
		map[string]any{"type": "tool_use", "id": "call-1", "name": "grep", "input": map[string]any{}},
	}, body.Messages[1].Content)
	assert.Equal(t, []any{
		map[string]any{"type": "tool_result", "tool_use_id": "call-1", "content": "--s1"},
		map[string]any{"type": "text", "text": "继续"},
	}, body.Messages[2].Content)
	assert.Equal(t, []any{map[string]any{"type": "tool_use", "id": "call-2", "name": "bash", "input": map[string]any{}}}, body.Messages[3].Content)
	assert.Equal(t, []any{map[string]any{"type": "tool_result", "tool_use_id": "call-2", "content": "ok"}}, body.Messages[4].Content)
}

// 字符串内容、非对象元素、原本为空的内容和缺失的 messages 都按原样透传。
func TestOMPAnthropicKeepsNonThinkingShapesUnchanged(t *testing.T) {
	for _, raw := range []string{
		`{"model":"alias","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[]},{"role":"user","content":["raw",{"type":"text","text":"x"}]}]}`,
		`{"model":"alias","messages":"not-array"}`,
		`{"model":"alias"}`,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/messages", strings.NewReader(raw))
		common.SetContextKey(c, constant.ContextKeyTworkOMPVersion, "18.12.2")
		data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "alias"}})
		require.NoError(t, err)
		var expected, actual map[string]any
		require.NoError(t, common.Unmarshal([]byte(raw), &expected))
		require.NoError(t, common.Unmarshal(data, &actual))
		assert.Equal(t, expected, actual)
	}
}
