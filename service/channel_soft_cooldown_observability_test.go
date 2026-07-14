package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
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

type blockingRedisHook struct {
	calls atomic.Int32
}

func (h *blockingRedisHook) BeforeProcess(ctx context.Context, _ redis.Cmder) (context.Context, error) {
	h.calls.Add(1)
	<-ctx.Done()
	return ctx, ctx.Err()
}

func (h *blockingRedisHook) AfterProcess(context.Context, redis.Cmder) error {
	return nil
}

func (h *blockingRedisHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}

func (h *blockingRedisHook) AfterProcessPipeline(context.Context, []redis.Cmder) error {
	return nil
}

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

func gatheredCounterLabelValues(t *testing.T, metricName string, matchLabels map[string]string, labelName string) []string {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	require.NoError(t, err)
	values := make([]string, 0)
	for _, family := range families {
		if family.GetName() != metricName {
			continue
		}
		for _, metric := range family.GetMetric() {
			labels := make(map[string]string, len(metric.GetLabel()))
			for _, label := range metric.GetLabel() {
				labels[label.GetName()] = label.GetValue()
			}
			matched := true
			for name, value := range matchLabels {
				if labels[name] != value {
					matched = false
					break
				}
			}
			if matched {
				values = append(values, labels[labelName])
			}
		}
	}
	return values
}

func useClosedRedisForSoftCooldownTest(t *testing.T) {
	t.Helper()

	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	require.NoError(t, client.Close())
	useRedisClientForSoftCooldownTest(t, client)
}

func useRedisClientForSoftCooldownTest(t *testing.T, client *redis.Client) {
	t.Helper()

	originalRDB := common.RDB
	originalRedisEnabled := common.RedisEnabled
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

func TestSoftCooldownRedisTimeoutTripsRequestCircuitBreaker(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	hook := &blockingRedisHook{}
	client := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1"})
	client.AddHook(hook)
	useRedisClientForSoftCooldownTest(t, client)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := newSoftCooldownObservabilityContext()

	startedAt := time.Now()
	_, firstCooling := GetChannelSoftCooldown(ctx, 91001, "claude-slow-redis")
	_, secondCooling := GetChannelSoftCooldown(ctx, 91002, "claude-slow-redis")
	elapsed := time.Since(startedAt)

	assert.False(t, firstCooling)
	assert.False(t, secondCooling)
	assert.Less(t, elapsed, time.Second)
	assert.Equal(t, int32(1), hook.calls.Load())
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(ctx, adminInfo)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, softCooldown["cache_degraded"])
}

func TestRecordChannelSoftCooldownAddsNestedAdminInfoAndAppliedMetric(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 17)
	const channelID = 91101
	const modelName = "claude-observability-applied"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)
	ctx := newSoftCooldownObservabilityContext()
	before := readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91101", "overloaded"))

	RecordChannelSoftCooldown(ctx, channelID, modelName, 529, "overloaded_error")
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(ctx, adminInfo)

	assert.Equal(t, before+1, readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91101", "overloaded")))
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, softCooldown["applied"])
	assert.Equal(t, 17, softCooldown["seconds"])
	assert.Equal(t, "overloaded", softCooldown["reason_class"])
	assert.Equal(t, []int{}, softCooldown["skipped_channel_ids"])
	assert.Equal(t, "local", softCooldown["cache_backend"])
	assert.Equal(t, false, softCooldown["cache_degraded"])
	assert.Equal(t, false, softCooldown["stream_semantic_committed"])
	assert.Equal(t, false, softCooldown["affinity_cleared"])
}

func TestRecordChannelSoftCooldownClassifiesErrorLabels(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 30)
	tests := []struct {
		name       string
		statusCode int
		errorType  string
		wantClass  string
	}{
		{name: "rate limit type", statusCode: http.StatusBadRequest, errorType: "rate_limit_error", wantClass: "rate_limit"},
		{name: "rate limit status", statusCode: http.StatusTooManyRequests, errorType: "vendor_limit_v2", wantClass: "rate_limit"},
		{name: "overloaded type", statusCode: http.StatusServiceUnavailable, errorType: "overloaded_error", wantClass: "overloaded"},
		{name: "overloaded status", statusCode: 529, errorType: "vendor_capacity_v3", wantClass: "overloaded"},
		{name: "generic server error", statusCode: http.StatusServiceUnavailable, errorType: "vendor_error_2026_07", wantClass: "server_error"},
		{name: "unknown", statusCode: http.StatusBadRequest, errorType: "vendor_error_2026_08", wantClass: "unknown"},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			channelID := 91110 + i
			modelName := fmt.Sprintf("claude-error-class-%d", i)
			cleanupChannelSoftCooldownKey(t, channelID, modelName)

			RecordChannelSoftCooldown(nil, channelID, modelName, tt.statusCode, tt.errorType)
			entry, cooling := GetChannelSoftCooldown(nil, channelID, modelName)

			require.True(t, cooling)
			assert.Equal(t, tt.wantClass, entry.ErrorClass)
			assert.Equal(t, []string{tt.wantClass}, gatheredCounterLabelValues(
				t,
				"newapi_channel_soft_cooldown_applied_total",
				map[string]string{"channel_id": fmt.Sprintf("%d", channelID)},
				"error_class",
			))
		})
	}
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
	appliedBefore := readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91301", "rate_limit"))

	RecordChannelSoftCooldown(ctx, 91301, "claude-cache-error", 429, "rate_limit_error")
	_, cooling := GetChannelSoftCooldown(ctx, 91301, "claude-cache-error")
	assert.False(t, cooling)

	assert.Equal(t, setBefore+1, readCounterValue(t, channelSoftCooldownCacheErrors.WithLabelValues("set")))
	assert.Equal(t, getBefore, readCounterValue(t, channelSoftCooldownCacheErrors.WithLabelValues("get")))
	assert.Equal(t, appliedBefore, readCounterValue(t, channelSoftCooldownApplied.WithLabelValues("91301", "rate_limit")))
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(ctx, adminInfo)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, false, softCooldown["applied"])
	assert.Equal(t, 30, softCooldown["seconds"])
	assert.Equal(t, "rate_limit", softCooldown["reason_class"])
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
