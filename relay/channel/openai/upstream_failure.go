package openai

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/types"
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
