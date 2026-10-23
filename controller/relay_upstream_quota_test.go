package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const moonshotSuspendedBody = `{"error":{"message":"Your account org-x <ak-x> is suspended due to insufficient balance, please recharge your account or check your plan and billing details","type":"exceeded_current_quota_error"}}`

func TestShouldRetryUpstreamQuotaExhaustedOutsideRetryStatusRanges(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 500, End: 599}})
	upstreamErr := types.WithOpenAIError(types.OpenAIError{
		Message: "用户额度不足, 剩余额度: ＄-0.005110",
		Type:    "new_api_error",
		Code:    "insufficient_user_quota",
	}, http.StatusForbidden)

	tests := []struct {
		name   string
		pinned bool
		want   bool
	}{
		{name: "普通请求换渠道重试", want: true},
		{name: "指定渠道仍不重试", pinned: true, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newRetryTestContext()
			if tt.pinned {
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "42")
			}
			assert.Equal(t, tt.want, shouldRetry(c, upstreamErr, 1))
		})
	}
}

// relayAgainstUpstream 让唯一渠道固定回给定的上游错误，返回客户端最终收到的响应。
func relayAgainstUpstream(t *testing.T, status int, upstreamBody string, format types.RelayFormat) *httptest.ResponseRecorder {
	t.Helper()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	httpClient := service.GetHttpClient()
	require.NotNil(t, httpClient)
	originalTransport := httpClient.Transport
	httpClient.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(upstreamBody)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { httpClient.Transport = originalTransport })

	originalLogConsumeEnabled := common.LogConsumeEnabled
	originalRedisEnabled := common.RedisEnabled
	originalRetryTimes := common.RetryTimes
	originalAutoDisable := common.AutomaticDisableChannelEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	quotaSetting := operation_setting.GetQuotaSetting()
	originalQuotaSetting := *quotaSetting
	originalModelRatios, err := common.Marshal(ratio_setting.GetModelRatioCopy())
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"upstream-quota-test":0}`))
	common.LogConsumeEnabled = false
	common.RedisEnabled = false
	common.RetryTimes = 0
	common.AutomaticDisableChannelEnabled = false
	constant.ErrorLogEnabled = false
	quotaSetting.EnableFreeModelPreConsume = false
	t.Cleanup(func() {
		common.LogConsumeEnabled = originalLogConsumeEnabled
		common.RedisEnabled = originalRedisEnabled
		common.RetryTimes = originalRetryTimes
		common.AutomaticDisableChannelEnabled = originalAutoDisable
		constant.ErrorLogEnabled = originalErrorLogEnabled
		*quotaSetting = originalQuotaSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(originalModelRatios)))
	})

	path, body := "/v1/chat/completions", `{"model":"upstream-quota-test","messages":[{"role":"user","content":"hello"}]}`
	if format == types.RelayFormatClaude {
		path, body = "/v1/messages", `{"model":"upstream-quota-test","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`
	}
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "upstream-quota-test")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	common.SetContextKey(c, constant.ContextKeyChannelId, 9601)
	common.SetContextKey(c, constant.ContextKeyChannelName, "upstream-quota-test")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://upstream.test")
	common.SetContextKey(c, constant.ContextKeyChannelRatio, float64(0))

	Relay(c, format)
	return w
}

func TestRelayRewritesChannelFailureForClient(t *testing.T) {
	tests := []struct {
		name     string
		status   int
		upstream string
		leaked   []string
	}{
		{name: "上游欠费", status: http.StatusTooManyRequests, upstream: moonshotSuspendedBody, leaked: []string{"quota", "insufficient", "balance", "余额", "额度", "suspended"}},
		{name: "上游 502", status: http.StatusBadGateway, upstream: `{"error":{"message":"The upstream service could not complete the request.","type":"upstream_error"}}`, leaked: []string{"could not complete"}},
	}
	for _, tt := range tests {
		for _, format := range []types.RelayFormat{types.RelayFormatOpenAI, types.RelayFormatClaude} {
			t.Run(tt.name+"/"+string(format), func(t *testing.T) {
				w := relayAgainstUpstream(t, tt.status, tt.upstream, format)

				require.Equal(t, http.StatusServiceUnavailable, w.Code)
				// Claude 错误体只有 type/message，错误码只在 OpenAI 格式里体现。
				if format == types.RelayFormatOpenAI {
					assert.Contains(t, w.Body.String(), "upstream_unavailable")
				}
				assert.Contains(t, w.Body.String(), service.UpstreamUnavailableMessage)
				body := strings.ToLower(w.Body.String())
				for _, leaked := range tt.leaked {
					assert.NotContains(t, body, leaked)
				}
			})
		}
	}
}

func TestRelayKeepsRequestSpecificUpstreamError(t *testing.T) {
	const contextTooLong = `{"error":{"message":"prompt is too long: 250000 tokens > 200000 maximum","type":"invalid_request_error"}}`
	w := relayAgainstUpstream(t, http.StatusBadRequest, contextTooLong, types.RelayFormatOpenAI)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "prompt is too long")
	assert.NotContains(t, w.Body.String(), service.UpstreamUnavailableMessage)
}
