package service

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/types"
)

// UpstreamUnavailableMessage 是渠道都试过仍失败时回给用户的统一文案；上游原文只进日志。
const UpstreamUnavailableMessage = "当前模型服务繁忙，请更换其他模型，或稍后重试。"

// NewUpstreamUnavailableError 构造统一的渠道故障终态：503，OpenAI 格式带 code=upstream_unavailable，两种格式的文案都附请求 ID。
func NewUpstreamUnavailableError() *types.NewAPIError {
	return types.NewErrorWithStatusCode(errors.New(UpstreamUnavailableMessage), types.ErrorCodeUpstreamUnavailable, http.StatusServiceUnavailable)
}

// 请求本身的问题：换渠道也一样，保留原文让客户端给对应提示（开新对话、改说法等）。
var requestSpecificFailureFragments = []string{
	"context_length_exceeded",
	"prompt is too long",
	"maximum context length",
	"context window",
	"input is too long",
	"too many input tokens",
	"content_filter",
	"content_policy",
	"content exists risk",
	"data_inspection_failed",
	"inappropriate content",
	"sensitive_words",
}

// 本网关在调用上游前后产生、但归因于渠道的错误。
var localChannelFailureCodes = map[types.ErrorCode]struct{}{
	types.ErrorCodeDoRequestFailed:        {},
	types.ErrorCodeGetChannelFailed:       {},
	types.ErrorCodeBadResponse:            {},
	types.ErrorCodeBadResponseBody:        {},
	types.ErrorCodeBadResponseStatusCode:  {},
	types.ErrorCodeReadResponseBodyFailed: {},
	types.ErrorCodeEmptyResponse:          {},
}

// IsChannelSideFailure 判断最终错误是否属于渠道侧故障（换模型或稍后重试能解决），
// 用户积分、权限、非法请求、上下文超长和内容审核不算。
func IsChannelSideFailure(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if IsUpstreamQuotaExhaustedError(err) {
		return true
	}
	if err.GetErrorType() == types.ErrorTypeNewAPIError {
		if types.IsChannelError(err) {
			return true
		}
		_, ok := localChannelFailureCodes[err.GetErrorCode()]
		return ok
	}
	switch err.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return false
	}
	oai := err.ToOpenAIError()
	text := normalizeModelHealthText(strings.Join([]string{oai.Type, openAIErrorCodeToString(oai.Code), oai.Message, err.Error()}, " "))
	return !containsAnyModelHealthFragment(text, requestSpecificFailureFragments)
}
