package controller

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/setting/operation_setting"
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
