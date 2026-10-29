package oaichat

import (
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeConversionStopParameters(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  []string
	}{
		{"absent", nil, nil}, {"string", "END", []string{"END"}},
		{"typed array", []string{"A", "B"}, []string{"A", "B"}},
		{"JSON array", []any{"A", "B"}, []string{"A", "B"}},
		{"mixed array", []any{"A", float64(1), nil, true, map[string]any{"x": 1}, "B"}, []string{"A", "B"}},
		{"non-string array", []any{1, nil, true}, []string{}},
		{"number", 12, nil}, {"object", map[string]any{"stop": "END"}, nil},
		{"empty string", "", []string{""}}, {"empty array", []string{}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var result *dto.ClaudeRequest
			require.NotPanics(t, func() {
				var err error
				result, err = OpenAIChatRequestToClaudeMessages(nil, dto.GeneralOpenAIRequest{Model: "claude-test", Stop: tc.value, Messages: []dto.Message{{Role: "user", Content: "hello"}}})
				require.NoError(t, err)
			})
			require.NotNil(t, result)
			assert.Equal(t, tc.want, result.StopSequences)
		})
	}
}

func TestClaudeConversionMergesAllAdjacentToolCalls(t *testing.T) {
	for _, tc := range []struct{ name, first, second, wantText string }{
		{"two tool messages", `"checking"`, `"continuing"`, "checking continuing"},
		{"empty tool placeholder", `null`, `"continuing"`, "continuing"},
		{"literal ellipsis", `"..."`, `"continuing"`, "... continuing"},
		{"both empty", `""`, `""`, "..."},
		{"text between calls", `"checking"`, `"continuing"`, "checking middle continuing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"model":"claude-test","messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":` + tc.first + `,"tool_calls":[{"id":"a","type":"function","function":{"name":"first","arguments":"{\"n\":1}"}}]},{"role":"assistant","content":` + tc.second + `,"tool_calls":[{"id":"b","type":"function","function":{"name":"second","arguments":"{\"n\":2}"}}]},{"role":"tool","tool_call_id":"a","content":"result a"},{"role":"tool","tool_call_id":"b","content":"result b"}]}`
			var req dto.GeneralOpenAIRequest
			require.NoError(t, common.UnmarshalJsonStr(raw, &req))
			if tc.name == "text between calls" {
				req.Messages = append(req.Messages[:2], append([]dto.Message{{Role: "assistant", Content: "middle"}}, req.Messages[2:]...)...)
			}
			result, err := OpenAIChatRequestToClaudeMessages(nil, req)
			require.NoError(t, err)
			wire, err := common.Marshal(result)
			require.NoError(t, err)
			var decoded struct {
				Messages []struct {
					Role    string
					Content any
				}
			}
			require.NoError(t, common.Unmarshal(wire, &decoded))
			require.Len(t, decoded.Messages, 3)
			blocks, ok := decoded.Messages[1].Content.([]any)
			require.True(t, ok)
			var ids, names []string
			var text string
			for _, entry := range blocks {
				block := entry.(map[string]any)
				if block["type"] == "tool_use" {
					ids = append(ids, block["id"].(string))
					names = append(names, block["name"].(string))
					assert.Equal(t, float64(len(ids)), block["input"].(map[string]any)["n"])
				}
				if block["type"] == "text" {
					text += block["text"].(string)
				}
			}
			assert.Equal(t, []string{"a", "b"}, ids)
			assert.Equal(t, []string{"first", "second"}, names)
			assert.Equal(t, tc.wantText, text)
			results := decoded.Messages[2].Content.([]any)
			require.Len(t, results, 2)
			assert.Equal(t, "a", results[0].(map[string]any)["tool_use_id"])
			assert.Equal(t, "b", results[1].(map[string]any)["tool_use_id"])
		})
	}
}

func TestClaudeConversionMixedReasoningAndText(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, reasoningField string
		delayedUsage                 bool
		finishTogether               bool
	}{
		{name: "first chunk", reasoningField: "reasoning_content"},
		{name: "after empty role chunk", prefix: `{"role":"assistant"}`, reasoningField: "reasoning_content"},
		{name: "after thinking", prefix: `{"reasoning_content":"before "}`, reasoningField: "reasoning_content"},
		{name: "after text", prefix: `{"content":"before "}`, reasoningField: "reasoning_content"},
		{name: "reasoning alias", reasoningField: "reasoning"},
		{name: "finish in mixed first chunk", reasoningField: "reasoning_content", finishTogether: true},
		{name: "finish in mixed later chunk", prefix: `{"role":"assistant"}`, reasoningField: "reasoning_content", finishTogether: true},
		{name: "mixed finish then usage-only", prefix: `{"role":"assistant"}`, reasoningField: "reasoning_content", finishTogether: true, delayedUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{}
			var events []*dto.ClaudeResponse
			var chunks []string
			if tc.prefix != "" {
				chunks = append(chunks, `{"choices":[{"index":0,"delta":`+tc.prefix+`}]}`)
			}
			mixed := `{"choices":[{"index":0,"delta":{"` + tc.reasoningField + `":"thinking","content":"answer"}`
			if tc.finishTogether {
				mixed += `,"finish_reason":"stop"`
			}
			mixed += `}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
			if tc.delayedUsage {
				mixed = strings.Replace(mixed, `,"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}`, "", 1)
			}
			chunks = append(chunks, mixed)
			if tc.delayedUsage {
				chunks = append(chunks, `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
			}
			if !tc.finishTogether {
				chunks = append(chunks, `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
			}
			for index, raw := range chunks {
				var chunk dto.ChatCompletionsStreamResponse
				require.NoError(t, common.UnmarshalJsonStr(raw, &chunk))
				info.SendResponseCount++
				events = append(events, StreamResponseOpenAI2Claude(&chunk, info)...)
				if tc.delayedUsage && index == 1 {
					assert.False(t, info.ClaudeConvertInfo.Done, "用量到达前不能发完成终态")
				}
			}
			active := map[int]string{}
			var thinking, text string
			var starts, stops, usageEvents int
			for _, event := range events {
				switch event.Type {
				case "message_start":
					starts++
				case "content_block_start":
					_, exists := active[event.GetIndex()]
					require.False(t, exists)
					active[event.GetIndex()] = event.ContentBlock.Type
				case "content_block_delta":
					kind, exists := active[event.GetIndex()]
					require.True(t, exists, "delta 必须有已打开的内容块")
					switch event.Delta.Type {
					case "thinking_delta":
						assert.Equal(t, "thinking", kind)
						thinking += *event.Delta.Thinking
					case "text_delta":
						assert.Equal(t, "text", kind)
						text += *event.Delta.Text
					}
				case "content_block_stop":
					_, exists := active[event.GetIndex()]
					require.True(t, exists, "内容块只能关闭一次")
					delete(active, event.GetIndex())
				case "message_delta":
					usageEvents++
					require.NotNil(t, event.Usage)
					assert.Equal(t, 7, event.Usage.InputTokens)
					assert.Equal(t, 3, event.Usage.OutputTokens)
					assert.Equal(t, "end_turn", *event.Delta.StopReason)
				case "message_stop":
					stops++
					assert.Empty(t, active)
				}
			}
			wantThinking, wantText := "thinking", "answer"
			if tc.name == "after thinking" {
				wantThinking = "before thinking"
			}
			if tc.name == "after text" {
				wantText = "before answer"
			}
			assert.Equal(t, wantThinking, thinking)
			assert.Equal(t, wantText, text)
			assert.Equal(t, 1, starts)
			assert.Equal(t, 1, stops)
			assert.Equal(t, 1, usageEvents)
			assert.Empty(t, active)
			assert.Empty(t, StreamResponseOpenAI2Claude(&dto.ChatCompletionsStreamResponse{}, info), "完成后不再重复输出终态")
		})
	}
}
