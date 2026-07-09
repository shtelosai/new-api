package service

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/types"
)

var softModelHealthErrorCodes = map[string]struct{}{
	"system_cpu_overloaded":    {},
	"system_memory_overloaded": {},
	"system_disk_overloaded":   {},
}

var softModelHealthErrorTypes = map[string]struct{}{
	"rate_limit_error": {},
	"overloaded_error": {},
}

var hardModelHealthErrorCodes = map[string]struct{}{
	"invalid_api_key":                 {},
	"account_deactivated":             {},
	"billing_not_active":              {},
	"pre_consume_token_quota_failed":  {},
	"insufficient_user_quota":         {},
	"insufficient_quota":              {},
	"usage_limit_reached":             {},
	"arrearage":                       {},
	"access_denied":                   {},
	"channel:invalid_key":             {},
	"channel:no_available_key":        {},
	"channel:param_override_invalid":  {},
	"channel:header_override_invalid": {},
	"channel:model_mapped_error":      {},
	"channel:aws_client_error":        {},
	"channel:response_time_exceeded":  {},
}

var hardModelHealthErrorTypes = map[string]struct{}{
	"authentication_error":  {},
	"permission_error":      {},
	"forbidden":             {},
	"forbidden_error":       {},
	"billing_error":         {},
	"subscription_error":    {},
	"invalid_request_error": {},
}

var hardModelHealthMessageFragments = []string{
	"invalid api key",
	"incorrect api key",
	"permission denied",
	"not authorized",
	"organization has been disabled",
	"credit balance is too low",
	"exceeded your current quota",
	"insufficient quota",
	"insufficient balance",
	"quota_exhausted",
	"billing_not_active",
	"account_deactivated",
}

var softModelHealthMessageFragments = []string{
	"upstream rate limit exceeded",
	"rate_limit_error",
	"rate_limit_exceeded",
	"resource_exhausted",
	"upstream service overloaded",
	"overloaded_error",
	"model_capacity_exhausted",
	"service temporarily unavailable",
	"upstream service temporarily unavailable",
	"no available account",
	"no available accounts",
	"all available accounts exhausted",
	"account is busy",
	"too many requests",
	"too many pending requests",
	"too many concurrent requests",
	"concurrency limit exceeded",
	"please retry later",
}

var softNewAPINoChannelFragments = []string{
	"no available channel",
	"failed to get available channel",
	"distributor",
	"无可用渠道",
	"可用渠道失败",
}

// IsSoftModelHealthError 判断错误是否属于上游临时不可用/限流/容量不足。
//
// 软错误不代表 (channel, model) 永久坏了；自动禁用流程会跳过这类错误。
// 超时和鉴权/余额类错误仍是硬失败。
func IsSoftModelHealthError(err *types.NewAPIError) bool {
	if err == nil {
		return false
	}

	openAIError := err.ToOpenAIError()
	errType := normalizeModelHealthText(openAIError.Type)
	errCode := normalizeModelHealthText(openAIErrorCodeToString(openAIError.Code))
	message := normalizeModelHealthText(strings.Join([]string{
		openAIError.Message,
		err.Error(),
	}, " "))

	if isTimeoutModelHealthError(err.StatusCode, errType, errCode, message) {
		return false
	}
	if isHardModelHealthError(errType, errCode, message) {
		return false
	}
	if _, ok := softModelHealthErrorCodes[errCode]; ok {
		return true
	}
	if _, ok := softModelHealthErrorTypes[errType]; ok {
		return true
	}
	if errCode == string(types.ErrorCodeModelNotFound) && containsAnyModelHealthFragment(message, softNewAPINoChannelFragments) {
		return true
	}
	if err.StatusCode == http.StatusTooManyRequests || err.StatusCode == 529 {
		return true
	}
	if err.StatusCode >= http.StatusInternalServerError && containsAnyModelHealthFragment(message, softModelHealthMessageFragments) {
		return true
	}

	return false
}

func isTimeoutModelHealthError(statusCode int, errType, errCode, message string) bool {
	return statusCode == http.StatusGatewayTimeout ||
		strings.Contains(errType, "timeout") ||
		strings.Contains(errCode, "timeout") ||
		strings.Contains(message, "deadline_exceeded") ||
		strings.Contains(message, "request timeout") ||
		strings.Contains(message, "probe timeout")
}

func isHardModelHealthError(errType, errCode, message string) bool {
	if _, ok := hardModelHealthErrorCodes[errCode]; ok {
		return true
	}
	if _, ok := hardModelHealthErrorTypes[errType]; ok {
		return true
	}
	return containsAnyModelHealthFragment(message, hardModelHealthMessageFragments)
}

func containsAnyModelHealthFragment(text string, fragments []string) bool {
	for _, fragment := range fragments {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func normalizeModelHealthText(text string) string {
	return strings.ToLower(strings.TrimSpace(text))
}

func openAIErrorCodeToString(code any) string {
	switch v := code.(type) {
	case nil:
		return ""
	case string:
		return v
	case types.ErrorCode:
		return string(v)
	default:
		return fmt.Sprint(v)
	}
}
