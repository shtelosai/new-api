package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
)

func TestIsChannelSideFailure(t *testing.T) {
	upstream := func(status int, errType, code, message string) *types.NewAPIError {
		return types.WithOpenAIError(types.OpenAIError{Message: message, Type: errType, Code: code}, status)
	}
	tests := []struct {
		name string
		err  *types.NewAPIError
		want bool
	}{
		{name: "上游 502", err: upstream(http.StatusBadGateway, "upstream_error", "", "The upstream service could not complete the request."), want: true},
		{name: "上游 429 限流", err: upstream(http.StatusTooManyRequests, "rate_limit_error", "", "Too many requests"), want: true},
		{name: "上游 401 凭证失效", err: upstream(http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key"), want: true},
		{name: "上游 404 模型不在该分组", err: upstream(http.StatusNotFound, "new_api_error", "model_not_found", `Model "claude-haiku-5-5" is not available for this group`), want: true},
		{name: "上游欠费", err: upstream(http.StatusTooManyRequests, "exceeded_current_quota_error", "", "suspended due to insufficient balance"), want: true},
		{name: "本网关连接上游失败", err: types.NewError(errors.New("dial tcp: timeout"), types.ErrorCodeDoRequestFailed), want: true},
		{name: "渠道配置错误", err: types.NewError(errors.New("no available key"), types.ErrorCodeChannelNoAvailableKey), want: true},
		{name: "重试时选不出渠道", err: types.NewError(errors.New("no untried channel"), types.ErrorCodeGetChannelFailed), want: true},
		{name: "上游 400 请求不合法", err: upstream(http.StatusBadRequest, "invalid_request_error", "", "messages: field required"), want: false},
		{name: "上游 413 请求太大", err: upstream(http.StatusRequestEntityTooLarge, "invalid_request_error", "", "request too large"), want: false},
		{name: "上下文超长但状态码是 500", err: upstream(http.StatusInternalServerError, "invalid_request_error", "", "prompt is too long: 250000 tokens > 200000 maximum"), want: false},
		{name: "内容审核", err: upstream(http.StatusForbidden, "data_inspection_failed", "", "Input data may contain inappropriate content."), want: false},
		{name: "用户自己积分不足", err: types.NewErrorWithStatusCode(errors.New("用户额度不足"), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden), want: false},
		{name: "本网关请求校验失败", err: types.NewError(errors.New("invalid request"), types.ErrorCodeInvalidRequest), want: false},
		{name: "空错误", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsChannelSideFailure(tt.err))
		})
	}
}

func TestChainedUpstreamUnavailableIsSoftFailure(t *testing.T) {
	// 上游也是本网关：它回的统一繁忙错误只冷却，不能被下游按 503 写成禁用。
	chained := types.WithOpenAIError(types.OpenAIError{Message: UpstreamUnavailableMessage, Type: "new_api_error", Code: "upstream_unavailable"}, http.StatusServiceUnavailable)
	assert.True(t, IsSoftModelHealthError(chained))
}
