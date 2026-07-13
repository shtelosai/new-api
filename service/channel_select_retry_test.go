package service

import (
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
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
