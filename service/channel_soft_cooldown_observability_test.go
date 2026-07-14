package service

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/go-redis/redis/v8"
	"github.com/prometheus/client_golang/prometheus"
	clientmodel "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newSoftCooldownObservabilityContext() *gin.Context {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	return ctx
}

func readCounterValue(t *testing.T, counter prometheus.Counter) float64 {
	t.Helper()
	metric := &clientmodel.Metric{}
	require.NoError(t, counter.Write(metric))
	return metric.GetCounter().GetValue()
}

func useClosedRedisForSoftCooldownTest(t *testing.T) {
	t.Helper()

	originalRDB := common.RDB
	originalRedisEnabled := common.RedisEnabled
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, client.Close())
	common.RDB = client
	common.RedisEnabled = true
	channelSoftCooldownCache = nil
	channelSoftCooldownCacheOnce = sync.Once{}

	t.Cleanup(func() {
		common.RDB = originalRDB
		common.RedisEnabled = originalRedisEnabled
		channelSoftCooldownCache = nil
		channelSoftCooldownCacheOnce = sync.Once{}
	})
}

func TestRecordChannelSoftCooldownAddsNestedAdminInfoAndAppliedMetric(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 17)
	const channelID = 91101
	const modelName = "claude-observability-applied"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)
	ctx := newSoftCooldownObservabilityContext()
	before := readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91101", "overloaded_error"))

	RecordChannelSoftCooldown(ctx, channelID, modelName, 529, "overloaded_error")
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(ctx, adminInfo)

	assert.Equal(t, before+1, readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91101", "overloaded_error")))
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, softCooldown["applied"])
	assert.Equal(t, 17, softCooldown["seconds"])
	assert.Equal(t, "overloaded_error", softCooldown["reason_class"])
	assert.Equal(t, []int{}, softCooldown["skipped_channel_ids"])
	assert.Equal(t, "local", softCooldown["cache_backend"])
	assert.Equal(t, false, softCooldown["cache_degraded"])
	assert.Equal(t, false, softCooldown["stream_semantic_committed"])
	assert.Equal(t, false, softCooldown["affinity_cleared"])
}

func TestAppendChannelSoftCooldownAdminInfoOnlyAddsRelatedRequests(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)

	unrelated := newSoftCooldownObservabilityContext()
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(unrelated, adminInfo)
	assert.NotContains(t, adminInfo, "soft_cooldown")

	committed := newSoftCooldownObservabilityContext()
	common.SetContextKey(committed, constant.ContextKeyClaudeStreamCommitted, true)
	adminInfo = map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(committed, adminInfo)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, softCooldown["stream_semantic_committed"])

	other := GenerateTextOtherInfo(committed, &relaycommon.RelayInfo{
		StartTime:         time.Now(),
		FirstResponseTime: time.Now(),
		ChannelMeta:       &relaycommon.ChannelMeta{},
	}, 1, 1, 1, 0, 1, 0, 1)
	consumeAdminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	assert.Contains(t, consumeAdminInfo, "soft_cooldown")

	operationSetting := operation_setting.GetChannelHealthSetting()
	operationSetting.SoftFailureCooldownEnabled = false
	disabled := newSoftCooldownObservabilityContext()
	common.SetContextKey(disabled, constant.ContextKeyClaudeStreamCommitted, true)
	adminInfo = map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(disabled, adminInfo)
	assert.NotContains(t, adminInfo, "soft_cooldown")
}

func TestSoftCooldownLogInfoAggregatesSkippedChannelsAndAffinityClear(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	ctx := newSoftCooldownObservabilityContext()
	common.SetContextKey(ctx, constant.ContextKeyClaudeStreamCommitted, true)

	recordSoftCooldownSkippedChannelForLog(ctx, 91201)
	recordSoftCooldownSkippedChannelForLog(ctx, 91201)
	recordSoftCooldownSkippedChannelForLog(ctx, 91202)
	RecordAffinityClearedForLog(ctx)
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(ctx, adminInfo)

	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, []int{91201, 91202}, softCooldown["skipped_channel_ids"])
	assert.Equal(t, true, softCooldown["affinity_cleared"])
	assert.Equal(t, true, softCooldown["stream_semantic_committed"])
}

func TestSoftCooldownCacheErrorsSetAdminInfoAndOperationMetrics(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	useClosedRedisForSoftCooldownTest(t)
	ctx := newSoftCooldownObservabilityContext()
	setBefore := readCounterValue(t, channelSoftCooldownCacheErrors.WithLabelValues("set"))
	getBefore := readCounterValue(t, channelSoftCooldownCacheErrors.WithLabelValues("get"))
	appliedBefore := readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91301", "rate_limit_error"))

	RecordChannelSoftCooldown(ctx, 91301, "claude-cache-error", 429, "rate_limit_error")
	_, cooling := GetChannelSoftCooldown(ctx, 91301, "claude-cache-error")
	assert.False(t, cooling)

	assert.Equal(t, setBefore+1, readCounterValue(t, channelSoftCooldownCacheErrors.WithLabelValues("set")))
	assert.Equal(t, getBefore+1, readCounterValue(t, channelSoftCooldownCacheErrors.WithLabelValues("get")))
	assert.Equal(t, appliedBefore, readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91301", "rate_limit_error")))
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(ctx, adminInfo)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "redis", softCooldown["cache_backend"])
	assert.Equal(t, true, softCooldown["cache_degraded"])
}

func TestRecordChannelSoftFailoverOutcomeUsesFixedLabelsOncePerRequest(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	outcomes := []string{
		SoftFailoverOutcomeSameRequestSuccess,
		SoftFailoverOutcomeDeferredAfterCommit,
		SoftFailoverOutcomeExhausted,
		SoftFailoverOutcomeAllCooling,
	}

	for i, outcome := range outcomes {
		t.Run(outcome, func(t *testing.T) {
			ctx := newSoftCooldownObservabilityContext()
			recordSoftCooldownSkippedChannelForLog(ctx, 91400+i)
			before := readCounterValue(t, channelSoftFailover.WithLabelValues(outcome))

			RecordChannelSoftFailoverOutcome(ctx, outcome)
			RecordChannelSoftFailoverOutcome(ctx, outcome)

			assert.Equal(t, before+1, readCounterValue(t, channelSoftFailover.WithLabelValues(outcome)))
		})
	}
}
