package openai

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// upstreamErrorStatus 上游回 2xx/3xx 却带着错误体时按 502 处理，否则重试与冷却会把它当成功。
func upstreamErrorStatus(status int) int {
	if status < http.StatusBadRequest {
		return http.StatusBadGateway
	}
	return status
}

// streamChunkError 取出流里一段 data 携带的上游错误；不是错误时返回 nil。
func streamChunkError(data string) *types.OpenAIError {
	// 热路径：正常增量不含 error 字段，先做字符串判断，避免每段都多解析一次 JSON。
	if !strings.Contains(data, `"error"`) {
		return nil
	}
	var probe struct {
		Error *types.OpenAIError `json:"error"`
	}
	if err := common.UnmarshalJsonStr(data, &probe); err != nil || probe.Error == nil {
		return nil
	}
	if probe.Error.Message == "" && probe.Error.Type == "" && probe.Error.Code == nil {
		return nil
	}
	return probe.Error
}

// streamChunkHasOutput 判断一段 Chat 增量是否带实际内容：正文、思考、工具调用或结束标记；只有角色、空增量、用量的不算。
func streamChunkHasOutput(data string) bool {
	var chunk dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
		return true // 认不出的分段保守视为已输出，不再重放。
	}
	for _, choice := range chunk.Choices {
		if choice.Delta.GetContentString() != "" || choice.Delta.GetReasoningContent() != "" || len(choice.Delta.ToolCalls) > 0 || (choice.FinishReason != nil && *choice.FinishReason != "") {
			return true
		}
	}
	return false
}

// streamEndedAbnormally 上游超时、读流出错或处理崩溃；客户端主动断开不算渠道故障。
func streamEndedAbnormally(info *relaycommon.RelayInfo) bool {
	if info == nil || info.StreamStatus == nil {
		return false
	}
	switch info.StreamStatus.EndReason {
	case relaycommon.StreamEndReasonTimeout, relaycommon.StreamEndReasonScannerErr, relaycommon.StreamEndReasonPanic:
		return true
	}
	return false
}

// writeStreamErrorAfterCommit 已写出正文后上游报错：原样发出错误帧，让客户端按断流处理。
func writeStreamErrorAfterCommit(c *gin.Context, info *relaycommon.RelayInfo, raw string, oaiErr *types.OpenAIError) {
	if info.RelayFormat == types.RelayFormatClaude {
		_ = helper.ClaudeData(c, dto.ClaudeResponse{Type: "error", Error: types.WithOpenAIError(*oaiErr, http.StatusBadGateway).ToClaudeError()})
		return
	}
	_ = helper.StringData(c, raw)
}

// streamChunkParsable 判断一段是不是合法 JSON；[DONE] 之类的结束标记不会走到这里。
func streamChunkParsable(data string) bool {
	return data == "" || json.Valid([]byte(data))
}

// writeStreamInterrupted 写出正文后上游断流：发客户端认得的断流错误帧，它会按断流从断点续写。
func writeStreamInterrupted(c *gin.Context, info *relaycommon.RelayInfo) {
	const message = "The upstream service reported an error during streaming."
	if info.RelayFormat == types.RelayFormatClaude {
		_ = helper.ClaudeData(c, dto.ClaudeResponse{Type: "error", Error: types.ClaudeError{Type: "upstream_stream_error", Message: message}})
		return
	}
	_ = helper.StringData(c, `{"error":{"message":"`+message+`","type":"upstream_error","code":"upstream_stream_error"}}`)
}

// 非空的结束原因：冒号前后可能有空白（有的上游按 Python 默认格式输出），null 与空字符串都不算。
var finishReasonPattern = regexp.MustCompile(`"finish_reason"\s*:\s*"[^"]`)

// hasFinishReason 判断这一段是否带非空的结束原因；热路径只做字符串与正则判断，不解析 JSON。
func hasFinishReason(data string) bool {
	return strings.Contains(data, `"finish_reason"`) && finishReasonPattern.MatchString(data)
}

// streamChunkUsage 取出这一段里上游报的有效用量；没有或无效返回 nil。
func streamChunkUsage(data string) *dto.Usage {
	if !strings.Contains(data, `"usage"`) || strings.Contains(data, `"usage":null`) {
		return nil
	}
	var chunk struct {
		Usage *dto.Usage `json:"usage"`
	}
	if err := common.UnmarshalJsonStr(data, &chunk); err != nil || chunk.Usage == nil || !service.ValidUsage(chunk.Usage) {
		return nil
	}
	return chunk.Usage
}

// isUsageOnlyChunk 只带用量、没有任何 choice 的分段（OpenAI 流末尾的用量包）。
func isUsageOnlyChunk(data string) bool {
	return strings.Contains(data, `"usage"`) && emptyChoicesPattern.MatchString(data)
}

var emptyChoicesPattern = regexp.MustCompile(`"choices"\s*:\s*\[\s*\]`)

// withoutFinishReason 去掉分段里的结束原因，保留正文；断流收尾补发尾段时用，避免格式转换生成成功终态。
func withoutFinishReason(data string) string {
	if !hasFinishReason(data) {
		return data
	}
	var chunk map[string]any
	if err := common.UnmarshalJsonStr(data, &chunk); err != nil {
		return data
	}
	choices, _ := chunk["choices"].([]any)
	for _, item := range choices {
		if choice, ok := item.(map[string]any); ok {
			choice["finish_reason"] = nil
		}
	}
	encoded, err := common.Marshal(chunk)
	if err != nil {
		return data
	}
	return string(encoded)
}

// usageOnlyChunk 拼一个只带用量的 Chat 分段，交给格式转换生成带用量的结束事件。
func usageOnlyChunk(id string, created int64, model string, usage *dto.Usage) string {
	encoded, err := common.Marshal(map[string]any{"id": id, "object": "chat.completion.chunk", "created": created, "model": model, "choices": []any{}, "usage": usage})
	if err != nil {
		return ""
	}
	return string(encoded)
}
