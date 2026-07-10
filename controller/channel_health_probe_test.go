package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type claudeAgentProbeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeAgentProbeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func setClaudeAgentProbeTestTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	originalTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = transport
	t.Cleanup(func() {
		http.DefaultClient.Transport = originalTransport
	})
}

func TestProbeModelWithImmediateRetriesRetriesFailuresUntilSuccess(t *testing.T) {
	attempts := 0

	result := probeModelWithImmediateRetries(context.Background(), &model.Channel{}, "test-model", 3, 20, func(_ context.Context, _ *model.Channel, _ string, _ int) probeResult {
		attempts++
		if attempts < 3 {
			return probeResult{success: false, errMsg: "temporary failure"}
		}
		return probeResult{success: true, latencyMs: 10}
	})

	assert.True(t, result.success)
	assert.Equal(t, 3, attempts)
}

func TestProbeModelWithImmediateRetriesStopsAfterConfirmedFailures(t *testing.T) {
	attempts := 0

	result := probeModelWithImmediateRetries(context.Background(), &model.Channel{}, "test-model", 3, 20, func(_ context.Context, _ *model.Channel, _ string, _ int) probeResult {
		attempts++
		return probeResult{success: false, errMsg: "failure"}
	})

	assert.False(t, result.success)
	assert.Equal(t, 3, attempts)
	assert.Equal(t, "failure", result.errMsg)
}

func TestProbeModelWithImmediateRetriesDoesNotRetryLocalError(t *testing.T) {
	attempts := 0

	result := probeModelWithImmediateRetries(context.Background(), &model.Channel{}, "test-model", 3, 20, func(_ context.Context, _ *model.Channel, _ string, _ int) probeResult {
		attempts++
		return probeResult{success: false, errMsg: "local unsupported", isLocalErr: true}
	})

	assert.True(t, result.isLocalErr)
	assert.Equal(t, 1, attempts)
}

func TestProbeModelWithImmediateRetriesRetriesSoftFailuresUntilConfirmed(t *testing.T) {
	attempts := 0

	result := probeModelWithImmediateRetries(context.Background(), &model.Channel{}, "test-model", 3, 20, func(_ context.Context, _ *model.Channel, _ string, _ int) probeResult {
		attempts++
		return probeResult{success: false, errMsg: "soft overload", isSoftErr: true}
	})

	assert.False(t, result.success)
	assert.True(t, result.isSoftErr)
	assert.Equal(t, 3, attempts)
}

func TestShouldSkipProbeHealthStateMachineKeepsSoftError(t *testing.T) {
	result := probeResult{success: false, errMsg: "soft overload", isSoftErr: true}

	assert.False(t, shouldSkipProbeHealthStateMachine(result, false))
}

func TestShouldSkipProbeHealthStateMachineSkipsLocalError(t *testing.T) {
	result := probeResult{success: false, errMsg: "local unsupported", isLocalErr: true}

	assert.True(t, shouldSkipProbeHealthStateMachine(result, false))
	assert.True(t, shouldSkipProbeHealthStateMachine(result, true))
}

func TestShouldSkipProbeHealthStateMachineKeepsTimeoutFailure(t *testing.T) {
	result := probeResult{success: false, errMsg: "probe timeout after 20s", timedOut: true}

	assert.False(t, shouldSkipProbeHealthStateMachine(result, false))
}

// passive_recovery：失败不进状态机（不改写禁用行），成功进状态机触发恢复
func TestShouldSkipProbeHealthStateMachineRecoveryOnlySkipsFailureKeepsSuccess(t *testing.T) {
	failure := probeResult{success: false, errMsg: "upstream error"}
	timeout := probeResult{success: false, errMsg: "probe timeout after 20s", timedOut: true}
	success := probeResult{success: true, latencyMs: 10}

	assert.True(t, shouldSkipProbeHealthStateMachine(failure, true))
	assert.True(t, shouldSkipProbeHealthStateMachine(timeout, true))
	assert.False(t, shouldSkipProbeHealthStateMachine(success, true))
}

// 探针超时读专用配置 probe_timeout_sec，不再误用 ChannelDisableThreshold（响应时间禁用阈值）
func TestGetModelProbeTimeoutSecUsesChannelHealthProbeTimeout(t *testing.T) {
	setting := operation_setting.GetChannelHealthSetting()
	origTimeout := setting.ProbeTimeoutSec
	origThreshold := common.ChannelDisableThreshold
	setting.ProbeTimeoutSec = 37
	common.ChannelDisableThreshold = 5
	t.Cleanup(func() {
		setting.ProbeTimeoutSec = origTimeout
		common.ChannelDisableThreshold = origThreshold
	})

	assert.Equal(t, 37, getModelProbeTimeoutSec())
}

func TestGetModelProbeTimeoutSecFallback(t *testing.T) {
	setting := operation_setting.GetChannelHealthSetting()
	orig := setting.ProbeTimeoutSec
	setting.ProbeTimeoutSec = 0
	t.Cleanup(func() {
		setting.ProbeTimeoutSec = orig
	})

	assert.Equal(t, 20, getModelProbeTimeoutSec())
}

func TestShouldProbeChannelModelsOnlyAnthropicChannels(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_PROBE_URL", "http://probe:3021")

	assert.True(t, shouldProbeChannelModels(&model.Channel{
		Type:   constant.ChannelTypeAnthropic,
		Status: common.ChannelStatusEnabled,
	}))
	assert.False(t, shouldProbeChannelModels(&model.Channel{
		Type:   constant.ChannelTypeOpenAI,
		Status: common.ChannelStatusEnabled,
	}))
	assert.False(t, shouldProbeChannelModels(&model.Channel{
		Type:   constant.ChannelTypeAnthropic,
		Status: common.ChannelStatusManuallyDisabled,
	}))
	assert.False(t, shouldProbeChannelModels(nil))
}

// passive 探活目标覆盖全部常规渠道类型（不再要求 Anthropic+探针）：
// manual 行、非启用渠道、孤儿行、Midjourney/视频类不支持渠道仍被排除。
func TestPassiveRecoveryProbeTargetsCoverAllRegularChannelTypes(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelDisabled{}))
	t.Setenv("CLAUDE_AGENT_PROBE_URL", "http://probe:3021")

	require.NoError(t, db.Create([]model.ChannelModelDisabled{
		{ChannelId: 1, Model: "claude-auto", Source: model.DisabledSourceAuto},
		{ChannelId: 1, Model: "claude-relay", Source: model.DisabledSourceRelay},
		{ChannelId: 1, Model: "claude-manual", Source: model.DisabledSourceManual},
		{ChannelId: 2, Model: "openai-relay", Source: model.DisabledSourceRelay},
		{ChannelId: 3, Model: "manual-channel", Source: model.DisabledSourceAuto},
		{ChannelId: 4, Model: "auto-disabled-channel", Source: model.DisabledSourceAuto},
		{ChannelId: 5, Model: "mj-model", Source: model.DisabledSourceRelay},
		{ChannelId: 6, Model: "orphan-model", Source: model.DisabledSourceRelay},
	}).Error)

	targets, err := buildPassiveRecoveryProbeTargets([]*model.Channel{
		{Id: 1, Type: constant.ChannelTypeAnthropic, Status: common.ChannelStatusEnabled},
		{Id: 2, Type: constant.ChannelTypeOpenAI, Status: common.ChannelStatusEnabled},
		{Id: 3, Type: constant.ChannelTypeAnthropic, Status: common.ChannelStatusManuallyDisabled},
		{Id: 4, Type: constant.ChannelTypeAnthropic, Status: common.ChannelStatusAutoDisabled},
		{Id: 5, Type: constant.ChannelTypeMidjourney, Status: common.ChannelStatusEnabled},
	})

	require.NoError(t, err)
	require.Len(t, targets, 3)
	assert.Equal(t, 1, targets[0].channel.Id)
	assert.Equal(t, "claude-auto", targets[0].model)
	assert.Equal(t, 1, targets[1].channel.Id)
	assert.Equal(t, "claude-relay", targets[1].model)
	assert.Equal(t, 2, targets[2].channel.Id)
	assert.Equal(t, "openai-relay", targets[2].model)
}

func TestGetModelHealthProbeEndpointTypeUsesAutoDetectEndpoint(t *testing.T) {
	assert.Empty(t, getModelHealthProbeEndpointType(&model.Channel{Type: constant.ChannelTypeAnthropic}))
	assert.Empty(t, getModelHealthProbeEndpointType(&model.Channel{Type: constant.ChannelTypeOpenAI}))
}

func TestShouldUseClaudeAgentProbeRequiresConfigAndAnthropic(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_PROBE_URL", "http://probe:3021")

	assert.True(t, shouldUseClaudeAgentProbe(&model.Channel{Type: constant.ChannelTypeAnthropic}))
	assert.False(t, shouldUseClaudeAgentProbe(&model.Channel{Type: constant.ChannelTypeOpenAI}))
	assert.False(t, shouldUseClaudeAgentProbe(nil))
}

func TestClaudeAgentProbeUnavailableReturnsLocalError(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_PROBE_URL", "http://probe.test")
	setClaudeAgentProbeTestTransport(t, claudeAgentProbeRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusInternalServerError,
			Header:     http.Header{"Content-Type": []string{"text/plain"}},
			Body:       io.NopCloser(strings.NewReader("not-json")),
		}, nil
	}))

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	result, handled := testChannelWithClaudeAgentProbe(c, &model.Channel{
		Id:   1,
		Type: constant.ChannelTypeAnthropic,
	}, &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelBaseUrl:    "http://upstream.test",
			ApiKey:            "sk-test",
			UpstreamModelName: "claude-test",
		},
	})

	assert.True(t, handled)
	assert.Error(t, result.localErr)
	assert.Nil(t, result.newAPIError)
}

func TestBuildClaudeAgentProbeCustomHeadersAppliesStaticOverrides(t *testing.T) {
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{
			ApiKey: "channel-key",
			HeadersOverride: map[string]interface{}{
				"Authorization": "Bearer {api_key}",
				"X-Test":        "ok",
				"*":             true,
				"X-Client":      "{client_header:X-Client}",
			},
		},
	}

	headers, apiKey, authToken := buildClaudeAgentProbeCustomHeaders(info)

	assert.Equal(t, "channel-key", apiKey)
	assert.Equal(t, "channel-key", authToken)
	assert.Equal(t, "ok", headers["X-Test"])
	assert.NotContains(t, headers, "X-Client")
}
