package helper

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponseChunkHasSemanticOutput(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want bool
	}{
		{"开始", `{"type":"response.created","response":{"output":[]}}`, false},
		{"排队", `{"type":"response.queued"}`, false},
		{"进行中", `{"type":"response.in_progress"}`, false},
		{"空消息", `{"type":"response.output_item.added","item":{"type":"message","content":[]}}`, false},
		{"空正文块", `{"type":"response.content_part.added","part":{"type":"output_text","text":""}}`, false},
		{"空思考项", `{"type":"response.output_item.added","item":{"type":"reasoning","summary":[],"encrypted_content":"opaque"}}`, false},
		{"空思考块", `{"type":"response.reasoning_summary_part.done","part":{"type":"summary_text","text":""}}`, false},
		{"空正文结束", `{"type":"response.output_text.done","text":""}`, false},
		{"空工具骨架", `{"type":"response.output_item.added","item":{"type":"function_call","call_id":"c1","arguments":""}}`, false},
		{"空参数增量", `{"type":"response.function_call_arguments.delta","delta":""}`, false},
		{"空结果正常结束", `{"type":"response.completed","response":{"output":[]}}`, false},
		{"正文增量", `{"type":"response.output_text.delta","delta":"正文"}`, true},
		{"正文结束", `{"type":"response.output_text.done","text":"正文"}`, true},
		{"完整消息", `{"type":"response.output_item.done","item":{"type":"message","content":[{"type":"output_text","text":"正文"}]}}`, true},
		{"拒绝文本", `{"type":"response.content_part.added","part":{"type":"refusal","refusal":"无法处理"}}`, true},
		{"思考增量", `{"type":"response.reasoning_summary_text.delta","delta":"思考"}`, true},
		{"完整思考项", `{"type":"response.output_item.done","item":{"type":"reasoning","summary":[{"type":"summary_text","text":"思考"}]}}`, true},
		{"命名工具调用", `{"type":"response.output_item.added","item":{"type":"function_call","name":"read","arguments":""}}`, true},
		{"参数增量", `{"type":"response.function_call_arguments.delta","delta":"{}"}`, true},
		{"完整参数", `{"type":"response.function_call_arguments.done","arguments":"{}"}`, true},
		{"内置工具", `{"type":"response.output_item.added","item":{"type":"web_search_call","status":"in_progress"}}`, true},
		{"终态携带正文", `{"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"正文"}]}]}}`, true},
		{"未知媒体保守处理", `{"type":"response.audio.delta","delta":"audio"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var event dto.ResponsesStreamResponse
			require.NoError(t, common.UnmarshalJsonStr(tc.data, &event))
			assert.Equal(t, tc.want, ResponseChunkHasSemanticOutput(event, tc.data))
		})
	}
}
