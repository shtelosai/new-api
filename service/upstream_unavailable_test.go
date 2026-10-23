package service

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestIsChannelSideFailureExcludesUserCausedUpstreamErrors(t *testing.T) {
	// Claude 流内错误一律包成 500：非法请求要按结构化类型认出来，不说成繁忙。
	streamInvalid := types.WithClaudeError(types.ClaudeError{Type: "invalid_request_error", Message: "messages.0.content: tool_use ids must be unique"}, http.StatusInternalServerError)
	assert.False(t, IsChannelSideFailure(streamInvalid))
	// 内容审核与违规费：已经可能收了违规费，不能提示「不是你的问题」。
	csam := types.WithOpenAIError(types.OpenAIError{Message: "Failed check: SAFETY_CHECK_TYPE_CSAM", Type: "invalid_request_error"}, http.StatusForbidden)
	assert.False(t, IsChannelSideFailure(csam))
	violation := types.NewErrorWithStatusCode(errors.New("违规"), types.ErrorCode("violation_fee.grok.csam"), http.StatusForbidden)
	assert.False(t, IsChannelSideFailure(violation))
	// 渠道不支持这次请求的协议：换个渠道就可能成功。
	assert.True(t, IsChannelSideFailure(types.NewError(errors.New("not implemented"), types.ErrorCodeChannelUnsupportedProtocol)))
}

func TestIsHardChannelFailure(t *testing.T) {
	upstream := func(status int, errType, code, message string) *types.NewAPIError {
		return types.WithOpenAIError(types.OpenAIError{Message: message, Type: errType, Code: code}, status)
	}
	tests := []struct {
		name string
		err  *types.NewAPIError
		want bool
	}{
		{name: "鉴权失败", err: upstream(http.StatusUnauthorized, "authentication_error", "invalid_api_key", "Invalid API key"), want: true},
		{name: "没开通该模型", err: upstream(http.StatusNotFound, "new_api_error", "model_not_found", "not available for this group"), want: true},
		{name: "上游欠费", err: upstream(http.StatusTooManyRequests, "exceeded_current_quota_error", "", "insufficient balance"), want: true},
		{name: "渠道没有可用密钥", err: types.NewError(errors.New("no keys"), types.ErrorCodeChannelNoAvailableKey), want: true},
		{name: "一次 500", err: upstream(http.StatusInternalServerError, "server_error", "", "internal error"), want: false},
		{name: "502", err: upstream(http.StatusBadGateway, "upstream_error", "", "bad gateway"), want: false},
		{name: "限流", err: upstream(http.StatusTooManyRequests, "rate_limit_error", "", "too many requests"), want: false},
		{name: "连接失败", err: types.NewError(errors.New("dial tcp"), types.ErrorCodeDoRequestFailed), want: false},
		{name: "协议不支持", err: types.NewError(errors.New("not implemented"), types.ErrorCodeChannelUnsupportedProtocol), want: false},
		{name: "请求本身的问题", err: upstream(http.StatusBadRequest, "invalid_request_error", "", "bad input"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsHardChannelFailure(tt.err))
		})
	}
}

func TestRecordChannelFailureCooldownCountsConsecutiveEpisodes(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	const channelID = 92001
	const modelName = "strikes-model"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)
	key := fmt.Sprintf("%d:%s", channelID, modelName)

	// 第一次失败冷却 1 次；冷却期内再失败不加。
	assert.Equal(t, 1, RecordChannelFailureCooldown(nil, channelID, modelName, 502, "upstream_error"))
	assert.Equal(t, 1, RecordChannelFailureCooldown(nil, channelID, modelName, 502, "upstream_error"))

	// 冷却到期后试探又失败才加一次。
	for want := 2; want <= 3; want++ {
		entry, found, err := getChannelSoftCooldownCache().Get(key)
		require.NoError(t, err)
		require.True(t, found)
		entry.ExpiresAt = time.Now().Add(-time.Second)
		require.NoError(t, getChannelSoftCooldownCache().SetWithTTL(key, entry, time.Minute))
		assert.Equal(t, want, RecordChannelFailureCooldown(nil, channelID, modelName, 502, "upstream_error"))
	}
}

func TestFixedChannelRequestsAreNotFailoverEligible(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	assert.True(t, ChannelFailoverEligible(c))
	common.SetContextKey(c, constant.ContextKeyTworkExplicitChannelRoute, true)
	assert.False(t, ChannelFailoverEligible(c), "用户指定了具体渠道，只走那个渠道")
}

func TestIsChannelSideFailureHonorsStructuredChannelMarkersOn400(t *testing.T) {
	// 有的上游用 400 回「没开通该模型 / 密钥无效」：仍是渠道的问题，而且明确坏掉。
	notEnabled := types.WithOpenAIError(types.OpenAIError{Message: "model not available", Type: "invalid_request_error", Code: "model_not_found"}, http.StatusBadRequest)
	assert.True(t, IsChannelSideFailure(notEnabled))
	assert.True(t, IsHardChannelFailure(notEnabled))
	badKey := types.WithOpenAIError(types.OpenAIError{Message: "bad key", Type: "invalid_request_error", Code: "invalid_api_key"}, http.StatusBadRequest)
	assert.True(t, IsHardChannelFailure(badKey))
}

func TestCoolingFallbackSharesProbeLeaseAcrossRequests(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	const channelID = 92101
	const modelName = "fallback-lease"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)
	key := fmt.Sprintf("%d:%s", channelID, modelName)
	RecordChannelFailureCooldown(nil, channelID, modelName, 502, "upstream_error")
	entry, found, err := getChannelSoftCooldownCache().Get(key)
	require.NoError(t, err)
	require.True(t, found)

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return c
	}
	first, second := newCtx(), newCtx()
	// 全冷却时同一渠道同一时刻只放一个请求过去。
	assert.True(t, claimCoolingFallback(first, key, entry))
	assert.False(t, claimCoolingFallback(second, key, entry))

	// 放行的这次试探失败：哪怕还在冷却期内也算新的一次。
	assert.Equal(t, 2, RecordChannelFailureCooldown(first, channelID, modelName, 502, "upstream_error"))
}

func TestModelHasConfiguredChannelsAcrossAutoGroups(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	cleanServiceTokenRoutingTables(t)
	require.NoError(t, model.DB.Create(&model.Ability{Group: "vip", Model: "configured-model", ChannelId: 1, Enabled: false}).Error)
	assert.True(t, model.ModelHasConfiguredChannels("auto", "configured-model", 0, false))
	assert.True(t, model.ModelHasConfiguredChannels("vip", "configured-model", 0, false), "禁用中的渠道也算配过")
	assert.False(t, model.ModelHasConfiguredChannels("default", "configured-model", 0, false))
	assert.False(t, model.ModelHasConfiguredChannels("auto", "never-configured-model", 0, false))
}

func TestExpiredCooldownProbeLeaseCannotBeBypassedAndSuccessClearsStrikes(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	const channelID = 92201
	const modelName = "probe-lease"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)
	key := fmt.Sprintf("%d:%s", channelID, modelName)
	RecordChannelFailureCooldown(nil, channelID, modelName, 502, "upstream_error")
	entry, _, err := getChannelSoftCooldownCache().Get(key)
	require.NoError(t, err)
	entry.ExpiresAt = time.Now().Add(-time.Second)
	entry.ProbeAfterExpiry = true
	require.NoError(t, getChannelSoftCooldownCache().SetWithTTL(key, entry, time.Minute))

	newCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return c
	}
	prober, other := newCtx(), newCtx()
	_, cooling := GetChannelSoftCooldown(prober, channelID, modelName)
	require.False(t, cooling, "第一个请求拿到试探名额")
	seen, cooling := GetChannelSoftCooldown(other, channelID, modelName)
	require.True(t, cooling)
	// 别的请求拿着读到的记录走「全冷却放行」，也拿不到第二个名额。
	assert.False(t, claimCoolingFallback(other, key, seen))
	assert.True(t, seen.AvailableAt().After(time.Now()))

	// 试探成功：整条记录清掉，连续次数归零。
	CompleteResponsesRecovery(prober, channelID, modelName)
	_, found, err := getChannelSoftCooldownCache().Get(key)
	require.NoError(t, err)
	assert.False(t, found)
}
