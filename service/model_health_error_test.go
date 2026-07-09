package service

import (
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsSoftModelHealthError_Sub2APIRateLimitAndOverload(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "rate limit type",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "Upstream rate limit exceeded, please retry later",
				Type:    "rate_limit_error",
			}, http.StatusTooManyRequests),
		},
		{
			name: "overloaded type",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "Upstream service overloaded, please retry later",
				Type:    "overloaded_error",
			}, http.StatusServiceUnavailable),
		},
		{
			name: "model capacity exhausted",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "MODEL_CAPACITY_EXHAUSTED",
				Type:    "upstream_error",
			}, http.StatusServiceUnavailable),
		},
		{
			name: "no available account",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "no available account",
				Type:    "api_error",
			}, http.StatusServiceUnavailable),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, IsSoftModelHealthError(tt.err))
		})
	}
}

func TestIsSoftModelHealthError_NewAPITemporaryErrors(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "direct system overloaded",
			err: types.NewErrorWithStatusCode(
				errors.New("system cpu overloaded"),
				"system_cpu_overloaded",
				http.StatusServiceUnavailable,
			),
		},
		{
			name: "upstream new-api no available channel",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "No available channel for model gpt-4o under group default (distributor)",
				Type:    "new_api_error",
				Code:    "model_not_found",
			}, http.StatusServiceUnavailable),
		},
		{
			name: "upstream new-api request rate limit",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "您已达到请求数限制：1分钟内最多请求10次",
				Type:    "new_api_error",
			}, http.StatusTooManyRequests),
		},
		{
			name: "sdk wrapped rate limit text",
			err: types.NewOpenAIError(
				errors.New(`claude agent probe failed: API Error: 429 {"type":"rate_limit_error","message":"too many requests"}`),
				types.ErrorCodeBadResponse,
				http.StatusBadGateway,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, IsSoftModelHealthError(tt.err))
		})
	}
}

func TestIsSoftModelHealthError_HardErrors(t *testing.T) {
	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "timeout is hard",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "upstream request timeout",
				Type:    "timeout_error",
			}, http.StatusGatewayTimeout),
		},
		{
			name: "invalid api key is hard",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "invalid api key",
				Type:    "authentication_error",
				Code:    "invalid_api_key",
			}, http.StatusUnauthorized),
		},
		{
			name: "quota exhausted is hard",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "You exceeded your current quota",
				Type:    "insufficient_quota",
			}, http.StatusTooManyRequests),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, IsSoftModelHealthError(tt.err))
		})
	}
}

func TestShouldDisableChannelSkipsSoftModelHealthErrorEvenWhenStatusMatches(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	origRanges := operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{
		{Start: 429, End: 429},
		{Start: 503, End: 503},
	}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = origRanges
	})

	tests := []struct {
		name string
		err  *types.NewAPIError
	}{
		{
			name: "rate limit",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "Upstream rate limit exceeded, please retry later",
				Type:    "rate_limit_error",
			}, http.StatusTooManyRequests),
		},
		{
			name: "temporary unavailable",
			err: types.WithOpenAIError(types.OpenAIError{
				Message: "Service temporarily unavailable",
				Type:    "api_error",
			}, http.StatusServiceUnavailable),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.False(t, ShouldDisableChannel(tt.err))
		})
	}
}

func TestShouldDisableChannelHardErrorHonorsStatusCodeConfig(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	origRanges := operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 401, End: 401}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = origRanges
	})

	err := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid api key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized)

	assert.True(t, ShouldDisableChannel(err))
}

func TestShouldDisableChannelSkipRetryWinsOverStatusCodeConfig(t *testing.T) {
	origEnabled := common.AutomaticDisableChannelEnabled
	origRanges := operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 401, End: 401}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = origRanges
	})

	err := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid api key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized, types.ErrOptionWithSkipRetry())

	assert.False(t, ShouldDisableChannel(err))
}

func TestShouldDisableChannelHardErrorHonorsGlobalSwitch(t *testing.T) {
	orig := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = false
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = orig
	})

	err := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid api key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized)

	require.False(t, ShouldDisableChannel(err))
}
