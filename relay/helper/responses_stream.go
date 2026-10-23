package helper

import (
	"github.com/QuantumNous/new-api/dto"
	"github.com/tidwall/gjson"
)

// ResponseChunkHasSemanticOutput 区分可丢弃的开始/空骨架事件与不能重放的输出。
// 未认识的事件保守地视为输出，避免新增工具或媒体协议被重复执行。
func ResponseChunkHasSemanticOutput(resp dto.ResponsesStreamResponse, data string) bool {
	root := gjson.Parse(data)
	switch resp.Type {
	case "response.created", "response.queued", "response.in_progress", "response.completed", "response.incomplete":
		for _, item := range root.Get("response.output").Array() {
			if responsesItemHasOutput(item) {
				return true
			}
		}
		return false
	case dto.ResponsesOutputTypeItemAdded, dto.ResponsesOutputTypeItemDone:
		return responsesItemHasOutput(root.Get("item"))
	case "response.content_part.added", "response.content_part.done", "response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return responsesPartHasOutput(root.Get("part"))
	case "response.output_text.delta", "response.refusal.delta", "response.reasoning_text.delta", "response.reasoning_summary_text.delta", "response.function_call_arguments.delta":
		return root.Get("delta").String() != ""
	case "response.output_text.done", "response.reasoning_text.done", "response.reasoning_summary_text.done":
		return root.Get("text").String() != ""
	case "response.refusal.done":
		return root.Get("refusal").String() != ""
	case "response.function_call_arguments.done":
		return root.Get("name").String() != "" || root.Get("arguments").String() != ""
	default:
		return true
	}
}

func responsesItemHasOutput(item gjson.Result) bool {
	switch item.Get("type").String() {
	case "message", "reasoning":
		for _, field := range []string{"content", "summary"} {
			for _, part := range item.Get(field).Array() {
				if responsesPartHasOutput(part) {
					return true
				}
			}
		}
		return false
	case "function_call":
		return item.Get("name").String() != "" || item.Get("arguments").String() != ""
	default:
		// 内置工具即使尚无结果也可能已执行；不能把它们当作空消息。
		return item.Exists()
	}
}

func responsesPartHasOutput(part gjson.Result) bool {
	switch part.Get("type").String() {
	case "output_text", "text", "summary_text", "reasoning_text":
		return part.Get("text").String() != ""
	case "refusal":
		return part.Get("refusal").String() != ""
	default:
		return part.Exists()
	}
}
