package service

import (
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
)

// UpstreamUnavailableMessage 是渠道都试过仍失败时回给用户的统一文案；上游原文只进日志。
const UpstreamUnavailableMessage = "当前模型服务繁忙，请更换其他模型，或稍后重试。"

// NewUpstreamUnavailableError 构造统一的渠道故障终态：503，OpenAI 与 Claude 两种回包的 type 都是 upstream_unavailable，
// 旧客户端与下一级网关都能据此识别；文案已附请求 ID。
func NewUpstreamUnavailableError(requestId string) *types.NewAPIError {
	return types.WithClaudeError(types.ClaudeError{
		Type:    string(types.ErrorCodeUpstreamUnavailable),
		Message: common.MessageWithRequestId(UpstreamUnavailableMessage, requestId),
	}, http.StatusServiceUnavailable)
}

// ChannelFailoverEligible 判断本次请求能否在渠道之间自动切换、冷却和试探恢复。
// 用户或管理员指定了具体渠道的请求只走那个渠道，失败也不影响其他人的选路。
func ChannelFailoverEligible(c *gin.Context) bool {
	if c == nil || !operation_setting.IsSoftFailureCooldownEnabled() {
		return false
	}
	if _, fixed := c.Get("specific_channel_id"); fixed {
		return false
	}
	if _, fixed := c.Get(string(constant.ContextKeyTokenSpecificChannelId)); fixed {
		return false
	}
	return !common.GetContextKeyBool(c, constant.ContextKeyTworkExplicitChannelRoute) &&
		!common.GetContextKeyBool(c, constant.ContextKeyTworkImageRoute)
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
	"violates usage guidelines",
	"safety_check_type",
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

// IsChannelSideFailure 判断错误是否属于渠道侧故障（换渠道、换模型或稍后重试能解决），
// 用户积分、权限、非法请求、上下文超长、内容审核与违规费不算。
func IsChannelSideFailure(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}
	if IsViolationFeeCode(err.GetErrorCode()) || HasCSAMViolationMarker(err) {
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
	oai := err.ToOpenAIError()
	errType := normalizeModelHealthText(oai.Type)
	errCode := normalizeModelHealthText(openAIErrorCodeToString(oai.Code))
	text := normalizeModelHealthText(strings.Join([]string{errType, errCode, oai.Message, err.Error()}, " "))
	if containsAnyModelHealthFragment(text, requestSpecificFailureFragments) {
		return false
	}
	// 渠道鉴权失败、没开通该模型：有的上游用 400 回，也是渠道的问题。
	if hasHardChannelMarker(errType, errCode) {
		return true
	}
	switch err.StatusCode {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return false
	}
	// Claude 流内错误一律包成 500，非法请求要按结构化类型认；模型不存在属于渠道没开通，不算非法请求。
	return !(errType == "invalid_request_error" && err.StatusCode != http.StatusNotFound)
}

func hasHardChannelMarker(errType, errCode string) bool {
	if _, ok := hardChannelFailureTypes[errType]; ok {
		return true
	}
	switch errCode {
	case "model_not_found", "invalid_api_key", "account_deactivated", "billing_not_active":
		return true
	}
	return false
}

// ChannelCooldownStrikesBeforeDisable 连续冷却这么多次仍失败，转为系统禁用，交给定时探测恢复。
const ChannelCooldownStrikesBeforeDisable = 3

// ShouldEscalateChannelFailure 可切换请求的渠道故障：明确坏掉，或连续冷却到次数，转系统禁用；一次 500 只冷却。
func ShouldEscalateChannelFailure(err *types.NewAPIError, strikes int) bool {
	if !common.AutomaticDisableChannelEnabled || !IsChannelSideFailure(err) {
		return false
	}
	return IsHardChannelFailure(err) || strikes >= ChannelCooldownStrikesBeforeDisable
}

// 渠道明确坏掉（鉴权、欠费、没开通该模型），等冷却也不会好。
var hardChannelFailureTypes = map[string]struct{}{
	"authentication_error": {},
	"permission_error":     {},
	"forbidden":            {},
	"forbidden_error":      {},
	"billing_error":        {},
	"subscription_error":   {},
	"not_found_error":      {},
}

// IsHardChannelFailure 判断渠道侧故障能否确定为持续性故障，是则直接转系统禁用，不等连续冷却。
func IsHardChannelFailure(err *types.NewAPIError) bool {
	if !IsChannelSideFailure(err) {
		return false
	}
	if IsUpstreamQuotaExhaustedError(err) {
		return true
	}
	if types.IsChannelError(err) {
		// 响应慢、协议不支持只是这次不合适，按冷却处理。
		code := err.GetErrorCode()
		return code != types.ErrorCodeChannelResponseTimeExceeded && code != types.ErrorCodeChannelUnsupportedProtocol
	}
	if err.GetErrorType() == types.ErrorTypeNewAPIError {
		return false
	}
	switch err.StatusCode {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	oai := err.ToOpenAIError()
	return hasHardChannelMarker(normalizeModelHealthText(oai.Type), normalizeModelHealthText(openAIErrorCodeToString(oai.Code)))
}
