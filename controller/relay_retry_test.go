package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newRetryTestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return c
}

func newCanceledRetryTestContext() *gin.Context {
	c := newRetryTestContext()
	requestCtx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestCtx)
	cancel()
	return c
}

type relayRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f relayRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func withRetryStatusRanges(t *testing.T, ranges []operation_setting.StatusCodeRange) {
	t.Helper()
	orig := operation_setting.AutomaticRetryStatusCodeRanges
	operation_setting.AutomaticRetryStatusCodeRanges = ranges
	t.Cleanup(func() { operation_setting.AutomaticRetryStatusCodeRanges = orig })
}

func TestShouldRetryRetriesGatewayTimeoutWhenRetryAvailable(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 500, End: 599}})
	c := newRetryTestContext()
	err := types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, http.StatusGatewayTimeout)

	require.True(t, shouldRetry(c, err, 1))
}

func TestShouldRetryRetriesCloudflareTimeoutWhenRetryAvailable(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 500, End: 599}})
	c := newRetryTestContext()
	err := types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, 524)

	require.True(t, shouldRetry(c, err, 1))
}

func TestShouldRetryDoesNotRetryAfterResponseStarted(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 500, End: 599}})
	c := newRetryTestContext()
	c.String(http.StatusOK, "partial")
	err := types.InitOpenAIError(types.ErrorCodeBadResponseStatusCode, http.StatusGatewayTimeout)

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryHonorsSkipRetryForGatewayTimeout(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 500, End: 599}})
	c := newRetryTestContext()
	err := types.NewErrorWithStatusCode(
		errors.New("upstream gateway timeout"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusGatewayTimeout,
		types.ErrOptionWithSkipRetry(),
	)

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryRetriesConfiguredUpstreamBadRequest(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 400, End: 499}})
	c := newRetryTestContext()
	err := types.NewOpenAIError(
		errors.New("upstream rejected the request"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)

	require.True(t, shouldRetry(c, err, 1))
}

func TestShouldRetryHonorsSkipRetryForUpstreamBadRequest(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 400, End: 499}})
	c := newRetryTestContext()
	err := types.NewOpenAIError(
		errors.New("invalid local request"),
		types.ErrorCodeInvalidRequest,
		http.StatusBadRequest,
		types.ErrOptionWithSkipRetry(),
	)

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryDoesNotRetryUpstreamBadRequestAfterResponseStarted(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 400, End: 499}})
	c := newRetryTestContext()
	c.String(http.StatusOK, "partial")
	err := types.NewOpenAIError(
		errors.New("upstream rejected the request"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryDoesNotRetryUpstreamBadRequestWithoutRetryBudget(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 400, End: 499}})
	c := newRetryTestContext()
	err := types.NewOpenAIError(
		errors.New("upstream rejected the request"),
		types.ErrorCodeBadResponseStatusCode,
		http.StatusBadRequest,
	)

	require.False(t, shouldRetry(c, err, 0))
}

func TestShouldRetryDoesNotRetryAlwaysSkipErrorCodeForBadRequest(t *testing.T) {
	withRetryStatusRanges(t, []operation_setting.StatusCodeRange{{Start: 400, End: 499}})
	c := newRetryTestContext()
	err := types.NewOpenAIError(
		errors.New("bad upstream response body"),
		types.ErrorCodeBadResponseBody,
		http.StatusBadRequest,
	)

	require.False(t, shouldRetry(c, err, 1))
}

func TestShouldRetryRetriesEmptyResponseBeforeWrite(t *testing.T) {
	c := newRetryTestContext()
	err := types.NewError(errors.New("upstream stream ended without any content"), types.ErrorCodeEmptyResponse)

	require.True(t, shouldRetry(c, err, 1))
}

func TestShouldRetryRetriesClaudeEmptyResponseAfterKeepalive(t *testing.T) {
	c := newRetryTestContext()
	_, writeErr := c.Writer.Write([]byte(": PING\n\n"))
	require.NoError(t, writeErr)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamGateActive, true)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, false)
	err := types.NewError(errors.New("upstream stream ended with zero usage and no output"), types.ErrorCodeEmptyResponse)

	require.True(t, shouldRetry(c, err, 1))
}

func TestShouldRetryDoesNotRetryClaudeErrorAfterSemanticCommit(t *testing.T) {
	c := newRetryTestContext()
	_, writeErr := c.Writer.Write([]byte("data: partial\n\n"))
	require.NoError(t, writeErr)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamGateActive, true)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, true)
	err := types.NewError(errors.New("upstream stream failed after output"), types.ErrorCodeEmptyResponse)

	require.False(t, shouldRetry(c, err, 1))
}

func TestRelayWritesClaudeStreamErrorAfterKeepalive(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, writeErr := c.Writer.Write([]byte(": PING\n\n"))
	require.NoError(t, writeErr)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamGateActive, true)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, false)

	Relay(c, types.RelayFormatClaude)

	require.Contains(t, w.Body.String(), "event: error\n")
	require.Contains(t, w.Body.String(), "data: {\"type\":\"error\"")
	require.NotContains(t, w.Body.String(), "\n{\"error\":")
}

func TestRelayWritesOpenAIStreamErrorAfterKeepalive(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	_, writeErr := c.Writer.Write([]byte(": PING\n\n"))
	require.NoError(t, writeErr)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamGateActive, true)
	common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, false)

	Relay(c, types.RelayFormatOpenAI)

	require.Contains(t, w.Body.String(), "data: {\"error\":")
	require.NotContains(t, w.Body.String(), "\n{\"error\":")
}

func TestRelayMarksSuccessfulClaudeRequest(t *testing.T) {
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	httpClient := service.GetHttpClient()
	require.NotNil(t, httpClient)
	originalTransport := httpClient.Transport
	httpClient.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(
				`{"id":"msg_test","type":"message","role":"assistant","model":"claude-3-haiku-20240307","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`,
			)),
			Request: req,
		}, nil
	})
	t.Cleanup(func() { httpClient.Transport = originalTransport })

	originalLogConsumeEnabled := common.LogConsumeEnabled
	quotaSetting := operation_setting.GetQuotaSetting()
	originalQuotaSetting := *quotaSetting
	originalModelRatios, err := common.Marshal(ratio_setting.GetModelRatioCopy())
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"claude-relay-success-test":0}`))
	common.LogConsumeEnabled = false
	quotaSetting.EnableFreeModelPreConsume = false
	t.Cleanup(func() {
		common.LogConsumeEnabled = originalLogConsumeEnabled
		*quotaSetting = originalQuotaSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(originalModelRatios)))
	})

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-relay-success-test","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "claude-relay-success-test")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	common.SetContextKey(c, constant.ContextKeyChannelId, 9501)
	common.SetContextKey(c, constant.ContextKeyChannelName, "claude-success-test")
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeAnthropic)
	common.SetContextKey(c, constant.ContextKeyChannelKey, "sk-test")
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://claude.test")
	common.SetContextKey(c, constant.ContextKeyChannelRatio, float64(0))

	Relay(c, types.RelayFormatClaude)

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"id":"msg_test"`)
	require.True(t, common.GetContextKeyBool(c, constant.ContextKeyClaudeRelaySucceeded))
}

func TestShouldRetryDoesNotRetryAfterClientCancel(t *testing.T) {
	c := newCanceledRetryTestContext()

	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "empty response",
			err:  types.NewError(errors.New("upstream stream ended without any content"), types.ErrorCodeEmptyResponse),
		},
		{
			name: "channel error",
			err:  types.NewError(errors.New("no available key"), types.ErrorCodeChannelNoAvailableKey),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.False(t, shouldRetry(c, tt.err, 1))
		})
	}
}

func TestShouldRetryTaskRelayDoesNotRetryAfterClientCancel(t *testing.T) {
	c := newCanceledRetryTestContext()
	err := &dto.TaskError{
		Code:       "upstream_timeout",
		Message:    "upstream timeout",
		StatusCode: http.StatusGatewayTimeout,
	}

	require.False(t, shouldRetryTaskRelay(c, 1, err, 1))
}

func TestRelayAttemptLimitOnlyExpandsClaudeWhenSoftCooldownEnabled(t *testing.T) {
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalSetting := *healthSetting
	originalRetryTimes := common.RetryTimes
	t.Cleanup(func() {
		*healthSetting = originalSetting
		common.RetryTimes = originalRetryTimes
	})
	healthSetting.SoftFailureMaxAttempts = 5
	common.RetryTimes = 0
	c := newRetryTestContext()

	healthSetting.SoftFailureCooldownEnabled = false
	require.Equal(t, 1, relayAttemptLimit(c, types.RelayFormatClaude))

	healthSetting.SoftFailureCooldownEnabled = true
	require.Equal(t, 5, relayAttemptLimit(c, types.RelayFormatClaude))
	require.Equal(t, 1, relayAttemptLimit(c, types.RelayFormatOpenAIResponses))

	c.Set(string(constant.ContextKeyTokenSpecificChannelId), "42")
	require.Equal(t, 1, relayAttemptLimit(c, types.RelayFormatClaude))

	c = newRetryTestContext()
	common.RetryTimes = 6
	require.Equal(t, 7, relayAttemptLimit(c, types.RelayFormatClaude), "开启冷却后不能缩小既有真实请求预算")
}

func TestRecordChannelSoftCooldownForRelayHonorsRoutingBoundaries(t *testing.T) {
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalSetting := *healthSetting
	originalRedisEnabled := common.RedisEnabled
	healthSetting.SoftFailureCooldownEnabled = true
	healthSetting.SoftFailureCooldownSeconds = 30
	common.RedisEnabled = false
	t.Cleanup(func() {
		*healthSetting = originalSetting
		common.RedisEnabled = originalRedisEnabled
	})

	softErr := types.WithOpenAIError(types.OpenAIError{
		Message: "upstream rate limit exceeded",
		Type:    "rate_limit_error",
		Code:    "rate_limit_error",
	}, http.StatusTooManyRequests)
	hardErr := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid api key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized)

	tests := []struct {
		name        string
		relayFormat types.RelayFormat
		info        *relaycommon.RelayInfo
		err         *types.NewAPIError
		setup       func(*gin.Context)
		enabled     bool
		wantCooling bool
	}{
		{name: "claude soft error", relayFormat: types.RelayFormatClaude, info: &relaycommon.RelayInfo{}, err: softErr, enabled: true, wantCooling: true},
		{name: "feature disabled", relayFormat: types.RelayFormatClaude, info: &relaycommon.RelayInfo{}, err: softErr, enabled: false},
		{name: "non claude", relayFormat: types.RelayFormatOpenAIResponses, info: &relaycommon.RelayInfo{}, err: softErr, enabled: true},
		{name: "specific channel", relayFormat: types.RelayFormatClaude, info: &relaycommon.RelayInfo{}, err: softErr, enabled: true, setup: func(c *gin.Context) {
			c.Set(string(constant.ContextKeyTokenSpecificChannelId), "42")
		}},
		{name: "admin channel test", relayFormat: types.RelayFormatClaude, info: &relaycommon.RelayInfo{IsChannelTest: true}, err: softErr, enabled: true},
		{name: "client canceled", relayFormat: types.RelayFormatClaude, info: &relaycommon.RelayInfo{}, err: softErr, enabled: true, setup: func(c *gin.Context) {
			requestCtx, cancel := context.WithCancel(c.Request.Context())
			c.Request = c.Request.WithContext(requestCtx)
			cancel()
		}},
		{name: "hard error", relayFormat: types.RelayFormatClaude, info: &relaycommon.RelayInfo{}, err: hardErr, enabled: true},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channelID := 9300 + i
			modelName := fmt.Sprintf("claude-record-boundary-%d", i)
			c := newRetryTestContext()
			if tt.setup != nil {
				tt.setup(c)
			}
			healthSetting.SoftFailureCooldownEnabled = tt.enabled

			recordChannelSoftCooldownForRelay(c, tt.relayFormat, tt.info, channelID, modelName, tt.err)

			healthSetting.SoftFailureCooldownEnabled = true
			entry, cooling := service.GetChannelSoftCooldown(channelID, modelName)
			require.Equal(t, tt.wantCooling, cooling)
			if tt.wantCooling {
				require.Equal(t, "rate_limit_error", entry.ErrorClass)
				require.Equal(t, http.StatusTooManyRequests, entry.StatusCode)
			}
		})
	}
}
