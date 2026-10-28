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

func buildTworkReasoningSummaryFixture(t *testing.T, version, path, raw string) map[string]any {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	common.SetContextKey(c, constant.ContextKeyTworkPiCompatibilityVersion, version)
	c.Request = httptest.NewRequest("POST", path, strings.NewReader(raw))
	data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{
		UpstreamModelName: "mapped-fixture",
		ChannelSetting:    dto.ChannelSettings{TworkWireAPI: "responses"},
	}})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, common.Unmarshal(data, &body))
	return body
}

func TestTworkReasoningSummaryPreservesTextAndToolHistoryForPiOne(t *testing.T) {
	body := buildTworkReasoningSummaryFixture(t, "1.0.0", "/v1/responses", `{
		"model":"public-fixture",
		"input":[
			{"role":"user","content":"原始任务"},
			{"type":"reasoning","id":"rs_fixture","encrypted_content":"fixture-only-opaque","content":null,"status":null,"summary":[
				{"type":"summary_text","text":"  第一段\n保留空白🙂  "},
				{"type":"summary_text","text":"第二段"}
			]},
			{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"read","arguments":"{ \"path\": \"./fixture.txt\", \"offset\": 0 }"},
			{"type":"function_call_output","id":"result_fixture","call_id":"call_fixture","output":"原始结果\n{\"marker\":\"fixture\"}"},
			{"type":"reasoning","id":"rs_later_fixture","encrypted_content":null,"summary":[{"type":"summary_text","text":"后续摘要"}]},
			{"role":"assistant","content":"已有回复"}
		]
	}`)
	assert.Equal(t, "mapped-fixture", body["model"])
	assert.Equal(t, false, body["store"])
	assert.Equal(t, []any{
		map[string]any{"role": "user", "content": "原始任务"},
		map[string]any{"role": "assistant", "content": "  第一段\n保留空白🙂  "},
		map[string]any{"role": "assistant", "content": "第二段"},
		map[string]any{"type": "function_call", "call_id": "call_fixture", "name": "read", "arguments": `{ "path": "./fixture.txt", "offset": 0 }`},
		map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": "原始结果\n{\"marker\":\"fixture\"}"},
		map[string]any{"role": "assistant", "content": "后续摘要"},
		map[string]any{"role": "assistant", "content": "已有回复"},
	}, body["input"])
}

func TestTworkReasoningSummaryOnlyReplaysNonemptySummaryTextForPiOne(t *testing.T) {
	for _, tc := range []struct {
		name, item string
		want       []any
	}{
		{"empty", `{"type":"reasoning","summary":[]}`, []any{}},
		{"null", `{"type":"reasoning","summary":null}`, []any{}},
		{"opaque_only", `{"type":"reasoning","encrypted_content":"fixture-only-opaque"}`, []any{}},
		{"native_content_only", `{"type":"reasoning","content":[{"type":"reasoning_text","text":"原生内容不作摘要回退"}]}`, []any{}},
		{"invalid_entries", `{"type":"reasoning","summary":[null,"invalid",{"type":"summary_text","text":null},{"type":"summary_text","text":7},{"type":"summary_text"},{"type":"summary_text","text":""},{"type":"output_text","text":"错误类型"},{"text":"缺类型"},{"type":"summary_text","text":"有效摘要"}]}`, []any{map[string]any{"role": "assistant", "content": "有效摘要"}}},
		{"nonempty_whitespace", `{"type":"reasoning","summary":[{"type":"summary_text","text":" \n\t"}]}`, []any{map[string]any{"role": "assistant", "content": " \n\t"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := buildTworkReasoningSummaryFixture(t, "1.0.0", "/v1/responses", `{"model":"public-fixture","input":[`+tc.item+`]}`)
			assert.Equal(t, tc.want, body["input"])
		})
	}
}

func TestTworkReasoningSummaryKeepsLegacyVersionHistoryUnchanged(t *testing.T) {
	for _, version := range []string{"0.85.1", "", "unrecognized-version"} {
		for _, path := range []string{"/v1/responses", "/v1/responses/compact"} {
			t.Run("version="+version+path, func(t *testing.T) {
				body := buildTworkReasoningSummaryFixture(t, version, path, `{
				"model":"public-fixture",
				"input":[
					{"role":"user","content":"旧版任务"},
					{"type":"reasoning","id":"rs_fixture","encrypted_content":null,"summary":[{"type":"summary_text","text":"旧版摘要"}]},
					{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"read","arguments":"{ \"path\": \"./fixture.txt\" }"},
					{"type":"function_call_output","call_id":"call_fixture","output":"旧版原结果"}
				]
			}`)
				assert.Equal(t, []any{
					map[string]any{"role": "user", "content": "旧版任务"},
					map[string]any{"type": "function_call", "call_id": "call_fixture", "name": "read", "arguments": `{ "path": "./fixture.txt" }`},
					map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": "旧版原结果"},
				}, body["input"])
			})
		}
	}
}

func TestTworkReasoningSummaryIsNotReplayedAsAssistantTextForOMP(t *testing.T) {
	// OMP 路由只开放 /v1/responses（不含 compact）；多段摘要、多个推理项都要整项丢弃。
	for _, version := range []string{"18.12.2", "1.0.0"} {
		t.Run(version, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			common.SetContextKey(c, constant.ContextKeyTworkOMPVersion, version)
			common.SetContextKey(c, constant.ContextKeyTworkPiCompatibilityVersion, "1.0.0")
			c.Request = httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{
				"model":"public-fixture",
				"input":[
					{"role":"user","content":"完善报告"},
					{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"我先核对旧版场景。"}]},
					{"type":"reasoning","id":"rs_fixture","encrypted_content":"fixture-only-opaque","summary":[{"type":"summary_text","text":"**Designing traceability report**\n\nI need to consider an extraction table."},{"type":"summary_text","text":"**Considering tool policy**\n\nI should edit surgically."}]},
					{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"eval","arguments":"{}"},
					{"type":"function_call_output","call_id":"call_fixture","output":"旧版 40 项"},
					{"type":"reasoning","id":"rs_later","summary":[{"type":"summary_text","text":"**Checking cross-chapter consistency**"}]}
				]
			}`))
			data, err := BuildTworkModelRequest(c, &RelayInfo{ChannelMeta: &ChannelMeta{
				UpstreamModelName: "mapped-fixture",
				ChannelSetting:    dto.ChannelSettings{TworkRuntime: "omp", TworkWireAPI: "responses"},
			}})
			require.NoError(t, err)
			for _, leaked := range []string{"Designing traceability report", "Considering tool policy", "Checking cross-chapter consistency", "fixture-only-opaque"} {
				assert.NotContains(t, string(data), leaked)
			}
			var body map[string]any
			require.NoError(t, common.Unmarshal(data, &body))
			assert.Equal(t, []any{
				map[string]any{"role": "user", "content": "完善报告"},
				map[string]any{"type": "message", "role": "assistant", "phase": "commentary", "content": []any{map[string]any{"type": "output_text", "text": "我先核对旧版场景。"}}},
				map[string]any{"type": "function_call", "call_id": "call_fixture", "name": "eval", "arguments": "{}"},
				map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": "旧版 40 项"},
			}, body["input"])
		})
	}
}

func TestTworkReasoningSummaryPreservesCompactFieldBoundaryForPiOne(t *testing.T) {
	body := buildTworkReasoningSummaryFixture(t, "1.0.0", "/v1/responses/compact", `{
		"model":"public-fixture","instructions":"压缩指令","store":true,"stream":true,
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"}}],"reasoning":{"effort":"high"},
		"input":[
			{"role":"user","content":"压缩任务"},
			{"type":"reasoning","id":"rs_fixture","encrypted_content":"fixture-only-opaque","summary":[{"type":"summary_text","text":"压缩前明文摘要"}]},
			{"type":"function_call","id":"fc_fixture","call_id":"call_fixture","name":"read","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_fixture","output":"压缩前工具结果"}
		]
	}`)
	assert.Equal(t, map[string]any{
		"model": "mapped-fixture", "instructions": "压缩指令",
		"input": []any{
			map[string]any{"role": "user", "content": "压缩任务"},
			map[string]any{"role": "assistant", "content": "压缩前明文摘要"},
			map[string]any{"type": "function_call", "call_id": "call_fixture", "name": "read", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call_fixture", "output": "压缩前工具结果"},
		},
	}, body)
}
