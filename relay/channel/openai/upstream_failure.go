package openai

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
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
		if choice.Delta.GetContentString() != "" || choice.Delta.GetReasoningContent() != "" || len(choice.Delta.ToolCalls) > 0 || choice.FinishReason != nil {
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
