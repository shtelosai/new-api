package service

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedRetryChannel(t *testing.T, id int, modelName string, priority int64) {
	t.Helper()
	weight := uint(30)
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:       id,
		Type:     1,
		Name:     "retry-channel",
		Key:      "sk-test",
		Status:   common.ChannelStatusEnabled,
		Models:   modelName,
		Group:    "default",
		Priority: &priority,
		Weight:   &weight,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     "default",
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

func newRetrySelectionParam(modelName string) *RetryParam {
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	return &RetryParam{
		Ctx:         ctx,
		TokenGroup:  "default",
		ModelName:   modelName,
		RequestPath: "/v1/messages",
		Retry:       common.GetPointer(0),
	}
}

func TestCacheGetRandomSatisfiedChannelDoesNotReuseFailedChannel(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })

	modelName := "retry-without-replacement"
	for _, channelID := range []int{3201, 3202} {
		seedRetryChannel(t, channelID, modelName, 95)
	}
	model.InitChannelCache()

	param := newRetrySelectionParam(modelName)
	first, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)

	param.ExcludeChannel(first.Id)
	param.SetRetry(1)
	second, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.NotEqual(t, first.Id, second.Id)
}

func TestCacheGetRandomSatisfiedChannelExhaustsSamePriorityBeforeFallback(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })

	modelName := "retry-priority-fallback"
	seedRetryChannel(t, 3211, modelName, 95)
	seedRetryChannel(t, 3212, modelName, 95)
	seedRetryChannel(t, 3213, modelName, 10)
	model.InitChannelCache()

	param := newRetrySelectionParam(modelName)
	param.ExcludeChannel(3211)
	param.SetRetry(1)

	channel, _, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3212, channel.Id)

	param.ExcludeChannel(3212)
	param.SetRetry(2)
	channel, _, err = CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3213, channel.Id)
}

func TestCacheGetRandomSatisfiedChannelReportsExhaustedCandidates(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })

	modelName := "retry-candidates-exhausted"
	seedRetryChannel(t, 3221, modelName, 95)
	model.InitChannelCache()

	param := newRetrySelectionParam(modelName)
	param.ExcludeChannel(3221)
	param.SetRetry(1)

	channel, _, err := CacheGetRandomSatisfiedChannel(param)
	assert.Nil(t, channel)
	require.ErrorIs(t, err, ErrNoUntriedChannel)
}

func TestCacheGetRandomSatisfiedChannelSoftCooldownDisabledPreservesSelection(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })
	setupChannelSoftCooldownTest(t, true, 30)

	modelName := "retry-cooldown-disabled"
	seedRetryChannel(t, 3231, modelName, 95)
	seedRetryChannel(t, 3232, modelName, 10)
	model.InitChannelCache()
	cleanupChannelSoftCooldownKey(t, 3231, modelName)
	RecordChannelSoftCooldown(nil, 3231, modelName, 529, "overloaded_error")
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = false

	param := newRetrySelectionParam(modelName)
	param.UseSoftFailureCooldown = true
	skippedBefore := readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3231"))
	channel, _, err := CacheGetRandomSatisfiedChannel(param)

	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3231, channel.Id)
	assert.Equal(t, 0, param.GetRetry())
	assert.Equal(t, skippedBefore, readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3231")))
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(param.Ctx, adminInfo)
	assert.NotContains(t, adminInfo, "soft_cooldown")
}

func TestCacheGetRandomSatisfiedChannelSkipsCoolingChannelWithoutConsumingRetry(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })
	setupChannelSoftCooldownTest(t, true, 30)

	modelName := "retry-skip-cooling"
	seedRetryChannel(t, 3241, modelName, 95)
	seedRetryChannel(t, 3242, modelName, 10)
	model.InitChannelCache()
	cleanupChannelSoftCooldownKey(t, 3241, modelName)
	RecordChannelSoftCooldown(nil, 3241, modelName, 529, "overloaded_error")

	param := newRetrySelectionParam(modelName)
	param.UseSoftFailureCooldown = true
	skippedBefore := readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3241"))
	channel, _, err := CacheGetRandomSatisfiedChannel(param)

	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3242, channel.Id)
	assert.Equal(t, 0, param.GetRetry(), "跳过冷却渠道不能消耗真实请求的重试计数")
	assert.Empty(t, param.ExcludedChannelIds, "冷却排除只能存在于本次选择调用内部")
	assert.Equal(t, skippedBefore+1, readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3241")))
	adminInfo := map[string]interface{}{}
	AppendChannelSoftCooldownAdminInfo(param.Ctx, adminInfo)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, []int{3241}, softCooldown["skipped_channel_ids"])
}

func TestCacheGetRandomSatisfiedChannelSkipsCoolingChannelInAutoGroup(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })
	setupChannelSoftCooldownTest(t, true, 30)

	modelName := "retry-auto-skip-cooling"
	seedRetryChannel(t, 3251, modelName, 95)
	seedRetryChannel(t, 3252, modelName, 10)
	model.InitChannelCache()
	cleanupChannelSoftCooldownKey(t, 3251, modelName)
	RecordChannelSoftCooldown(nil, 3251, modelName, 529, "overloaded_error")

	param := newRetrySelectionParam(modelName)
	param.TokenGroup = "auto"
	param.UseSoftFailureCooldown = true
	channel, group, err := CacheGetRandomSatisfiedChannel(param)

	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 3252, channel.Id)
	assert.Equal(t, "default", group)
	assert.Equal(t, 0, param.GetRetry())
}

func TestCacheGetRandomSatisfiedChannelReportsAllCandidatesCooling(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previousMemoryCacheEnabled := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previousMemoryCacheEnabled })
	setupChannelSoftCooldownTest(t, true, 30)

	modelName := "retry-all-cooling"
	seedRetryChannel(t, 3261, modelName, 95)
	seedRetryChannel(t, 3262, modelName, 10)
	model.InitChannelCache()
	now := time.Now()
	for channelID, expiresAt := range map[int]time.Time{
		3261: now.Add(5 * time.Second),
		3262: now.Add(2 * time.Second),
	} {
		cleanupChannelSoftCooldownKey(t, channelID, modelName)
		require.NoError(t, getChannelSoftCooldownCache().SetWithTTL(
			fmt.Sprintf("%d:%s", channelID, modelName),
			ChannelSoftCooldownEntry{ExpiresAt: expiresAt, StatusCode: 529, ErrorClass: "overloaded_error"},
			30*time.Second,
		))
	}

	param := newRetrySelectionParam(modelName)
	param.UseSoftFailureCooldown = true
	allCoolingBefore := readCounterValue(t, channelSoftFailover.WithLabelValues(SoftFailoverOutcomeAllCooling))
	firstSkippedBefore := readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3261"))
	secondSkippedBefore := readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3262"))
	channel, _, err := CacheGetRandomSatisfiedChannel(param)

	assert.Nil(t, channel)
	require.ErrorIs(t, err, ErrAllChannelsCooling)
	var coolingErr *AllChannelsCoolingError
	require.ErrorAs(t, err, &coolingErr)
	assert.InDelta(t, 2, coolingErr.RetryAfterSeconds(), 1)
	assert.Equal(t, 0, param.GetRetry())
	assert.Empty(t, param.ExcludedChannelIds)
	assert.Equal(t, allCoolingBefore+1, readCounterValue(t, channelSoftFailover.WithLabelValues(SoftFailoverOutcomeAllCooling)))
	assert.Equal(t, firstSkippedBefore+1, readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3261")))
	assert.Equal(t, secondSkippedBefore+1, readCounterValue(t, channelSoftCooldownSkipped.WithLabelValues("3262")))
}

func TestAllChannelsCoolingErrorRetryAfterUsesEarliestExpiry(t *testing.T) {
	err := &AllChannelsCoolingError{earliestExpiresAt: time.Now().Add(1500 * time.Millisecond)}
	assert.Equal(t, 2, err.RetryAfterSeconds())

	err.earliestExpiresAt = time.Now().Add(-time.Second)
	assert.Equal(t, 1, err.RetryAfterSeconds())
}
