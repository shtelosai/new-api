package common

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkChatModelCompatibilityAndPublicHistory(t *testing.T) {
	for _, tc := range []struct {
		profile, name, effort string
		expected              map[string]any
	}{
		{"deepseek", "unknown-alias", "none", map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"zai", "unknown-alias", "none", map[string]any{"thinking": map[string]any{"type": "disabled"}}},
		{"moonshotai-cn", "kimi-k2.6", "high", map[string]any{"thinking": map[string]any{"type": "enabled"}}},
	} {
		t.Run(tc.profile, func(t *testing.T) {
			body := map[string]any{"reasoning_effort": tc.effort, "store": false, "max_completion_tokens": float64(16),
				"messages": []any{map[string]any{"role": "assistant", "content": nil, "reasoning_details": []any{map[string]any{"data": "old-channel-encrypted"}}, "tool_calls": []any{map[string]any{"id": "call-a", "type": "function", "function": map[string]any{"name": "read", "arguments": "{}"}}}}, map[string]any{"role": "tool", "tool_call_id": "call-a", "content": "result"}},
				"tools":    []any{map[string]any{"type": "function", "function": map[string]any{"name": "read", "strict": true}}}}
			info := &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: tc.name, ChannelSetting: dto.ChannelSettings{TworkPiCompatibility: tc.profile}}}
			adaptTworkChatRequest(body, info, "")
			assert.NotContains(t, body, "reasoning_effort")
			assert.Equal(t, tc.expected["thinking"], body["thinking"])
			assert.NotContains(t, body, "store")
			assert.Equal(t, float64(16), body["max_tokens"])
			messages := body["messages"].([]any)
			assert.NotContains(t, messages[0].(map[string]any), "reasoning_details")
			assert.Equal(t, "call-a", messages[1].(map[string]any)["tool_call_id"])
			if tc.profile == "deepseek" {
				assert.Equal(t, "", messages[0].(map[string]any)["reasoning_content"])
			}
			if tc.profile == "moonshotai-cn" {
				assert.NotContains(t, body["tools"].([]any)[0].(map[string]any)["function"], "strict")
			}
		})
	}
}

func TestTworkResponsesKeepsToolPairingWithoutChannelRecoveryState(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"public-model","input":[{"type":"reasoning","encrypted_content":"old-channel"},{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"ok"}]}`))
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "mapped", ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses"}}})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	assert.Equal(t, "mapped", body["model"])
	assert.Equal(t, false, body["store"])
	input := body["input"].([]any)
	require.Len(t, input, 2)
	assert.NotContains(t, input[0], "id")
	assert.Equal(t, "call_1", input[0].(map[string]any)["call_id"])
	assert.Equal(t, "call_1", input[1].(map[string]any)["call_id"])
}

func TestTworkResponsesRejectsOpaqueChannelHistory(t *testing.T) {
	for _, input := range []string{
		`{"previous_response_id":"resp_old","input":"hi"}`,
		`{"input":[{"type":"item_reference","id":"msg_old"}]}`,
		`{"input":[{"type":"compaction","encrypted_content":"old-channel"}]}`,
	} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(input))
		_, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses"}}})
		require.Error(t, err)
	}
}

func TestTworkResponsesCompactDoesNotAddUnsupportedStore(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses/compact", strings.NewReader(`{"model":"public","input":"hi"}`))
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses"}}})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	assert.NotContains(t, body, "store")
}

func TestTworkChatSystemPromptPreservesArrayContent(t *testing.T) {
	body := map[string]any{"messages": []any{map[string]any{"role": "system", "content": []any{map[string]any{"type": "text", "text": "原提示"}}}}}
	info := &RelayInfo{ChannelMeta: &ChannelMeta{ChannelSetting: dto.ChannelSettings{SystemPrompt: "渠道提示", SystemPromptOverride: true}}}
	adaptTworkChatRequest(body, info, "")
	content := body["messages"].([]any)[0].(map[string]any)["content"]
	assert.Equal(t, []any{map[string]any{"type": "text", "text": "渠道提示"}, map[string]any{"type": "text", "text": "原提示"}}, content)
}

func TestTworkChatTemplateUsesNativeThinkingVariables(t *testing.T) {
	body := map[string]any{"reasoning_effort": "high"}
	adaptTworkChatRequest(body, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "moonshotai/Kimi-K2.5", ChannelSetting: dto.ChannelSettings{TworkPiCompatibility: "baseten"}}}, "")
	assert.Equal(t, map[string]any{"enable_thinking": true}, body["chat_template_args"])
}

func TestTworkResponsesAppliesNativeThinkingLevels(t *testing.T) {
	for _, tc := range []struct{ requested, expected string }{{"max", "high"}, {"none", "minimal"}} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"public","input":"hi","reasoning":{"effort":"`+tc.requested+`"}}`))
		data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-5", ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses", TworkPiCompatibility: "openai"}}})
		require.NoError(t, err)
		var body map[string]any
		require.NoError(t, common.Unmarshal(data, &body))
		assert.Equal(t, tc.expected, body["reasoning"].(map[string]any)["effort"])
	}
}

func TestTworkNativeCatalogDoesNotOverrideExplicitModelCapabilities(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"custom-gpt","input":"hi","reasoning":{"effort":"high"}}`))
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-4o", ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses", TworkPiCompatibility: "openai"}}})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	assert.Equal(t, map[string]any{"effort": "high"}, body["reasoning"])
}

func TestTworkNativeCatalogDoesNotInventThinkingParameters(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"custom-gpt","input":"hi"}`))
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "gpt-4o", ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses", TworkPiCompatibility: "openai"}}})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	assert.NotContains(t, body, "reasoning")
}

func TestTworkChatToolStreamFollowsSelectedNativeProfile(t *testing.T) {
	for _, tc := range []struct {
		profile             string
		withTools, expected bool
	}{
		{"zai", true, true}, {"zai", false, false}, {"", true, false},
	} {
		body := map[string]any{}
		if tc.withTools {
			body["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": "read"}}}
		}
		adaptTworkChatRequest(body, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: "glm-5.3-flash", ChannelSetting: dto.ChannelSettings{TworkPiCompatibility: tc.profile}}}, "")
		if tc.expected {
			assert.Equal(t, true, body["tool_stream"])
		} else {
			assert.NotContains(t, body, "tool_stream")
		}
	}
}

func TestTworkChatCompatibilityVersionPreservesExactModelSemantics(t *testing.T) {
	for _, tc := range []struct {
		profile, model, tokenField string
		legacyStrict, newStrict    bool
	}{
		{"moonshotai-cn", "kimi-k2.5", "max_tokens", false, false},
		{"zai-coding-cn", "glm-5.1", "max_tokens", true, false},
		{"bailian", "ZHIPU/GLM-5.3-Flash", "max_tokens", true, false},
		{"bailian", "unverified-alias", "max_completion_tokens", true, false},
		{"", "unverified-alias", "max_completion_tokens", true, false},
	} {
		for _, version := range []string{"0.85.1", "1.0.0"} {
			t.Run(tc.profile+"/"+tc.model+"/"+version, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				common.SetContextKey(c, constant.ContextKeyTworkPiCompatibilityVersion, version)
				c.Request = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"public-model","max_completion_tokens":16,"reasoning_effort":"max","messages":[{"role":"user","content":[{"type":"text","text":"检查图片"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aW1hZ2U="}}]},{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"工具结果"}],"tools":[{"type":"function","function":{"name":"read","strict":true}}]}`))
				data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: tc.model, ChannelSetting: dto.ChannelSettings{TworkWireAPI: "chat_completions", TworkPiCompatibility: tc.profile}}})
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, common.Unmarshal(data, &body))
				assert.Equal(t, tc.model, body["model"])
				assert.Equal(t, float64(16), body[tc.tokenField])
				otherField := "max_tokens"
				if tc.tokenField == otherField {
					otherField = "max_completion_tokens"
				}
				assert.NotContains(t, body, otherField)
				strict := tc.legacyStrict
				if version == "1.0.0" {
					strict = tc.newStrict
				}
				function := body["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)
				if strict {
					assert.Equal(t, true, function["strict"])
				} else {
					assert.NotContains(t, function, "strict")
				}
				switch tc.profile {
				case "moonshotai-cn":
					if version == "0.85.1" {
						assert.Equal(t, map[string]any{"type": "enabled"}, body["thinking"])
					} else {
						assert.NotContains(t, body, "thinking")
					}
					assert.NotContains(t, body, "reasoning_effort")
				case "zai-coding-cn":
					assert.Equal(t, map[string]any{"type": "enabled", "clear_thinking": false}, body["thinking"])
					if version == "0.85.1" {
						assert.Equal(t, true, body["tool_stream"])
					} else {
						assert.NotContains(t, body, "tool_stream")
					}
				case "bailian":
					assert.Equal(t, true, body["enable_thinking"])
				default:
					assert.Equal(t, "max", body["reasoning_effort"])
				}
				messages := body["messages"].([]any)
				content := messages[0].(map[string]any)["content"].([]any)
				assert.Equal(t, "data:image/png;base64,aW1hZ2U=", content[1].(map[string]any)["image_url"].(map[string]any)["url"])
				assert.Equal(t, "call_1", messages[2].(map[string]any)["tool_call_id"])
				assert.Equal(t, "工具结果", messages[2].(map[string]any)["content"])
			})
		}
	}
}

func TestTworkResponsesCompatibilityVersionRebuildsOriginalEffortOnRetry(t *testing.T) {
	for _, version := range []string{"0.85.1", "1.0.0"} {
		t.Run(version, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyTworkPiCompatibilityVersion, version)
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"public-model","reasoning":{"effort":"max"},"max_output_tokens":16,"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"检查图片"},{"type":"input_image","image_url":"data:image/png;base64,aW1hZ2U="}]},{"type":"function_call","call_id":"call_1","name":"read","arguments":"{}"},{"type":"function_call_output","call_id":"call_1","output":"工具结果"}]}`))
			for _, tc := range []struct{ profile, model, effort string }{
				{"openai", "gpt-5", "high"},
				{"opencode", "muse-spark-1.3", map[string]string{"0.85.1": "max", "1.0.0": "xhigh"}[version]},
			} {
				data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{UpstreamModelName: tc.model, ChannelSetting: dto.ChannelSettings{TworkWireAPI: "responses", TworkPiCompatibility: tc.profile}}})
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, common.Unmarshal(data, &body))
				assert.Equal(t, tc.model, body["model"])
				assert.Equal(t, tc.effort, body["reasoning"].(map[string]any)["effort"])
				assert.Equal(t, float64(16), body["max_output_tokens"])
				assert.Equal(t, false, body["store"])
				input := body["input"].([]any)
				require.Len(t, input, 3)
				assert.Equal(t, "call_1", input[1].(map[string]any)["call_id"])
				assert.Equal(t, "call_1", input[2].(map[string]any)["call_id"])
				assert.Equal(t, "工具结果", input[2].(map[string]any)["output"])
				content := input[0].(map[string]any)["content"].([]any)
				assert.Equal(t, "data:image/png;base64,aW1hZ2U=", content[1].(map[string]any)["image_url"])
			}
		})
	}
}
