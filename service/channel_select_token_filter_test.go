package service

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanServiceTokenRoutingTables(t *testing.T) {
	t.Helper()
	for _, table := range []string{"token_model_channels", "channels", "abilities"} {
		require.NoError(t, model.DB.Exec("DELETE FROM "+table).Error)
	}
	t.Cleanup(func() {
		for _, table := range []string{"token_model_channels", "channels", "abilities"} {
			require.NoError(t, model.DB.Exec("DELETE FROM "+table).Error)
		}
	})
}

func TestCacheGetRandomSatisfiedChannelUsesRetryParamTokenWhitelist(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	prevMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = prevMemory })

	modelName := "f2-retry-model"
	highPriority := int64(10)
	lowPriority := int64(0)
	for _, ch := range []*model.Channel{
		{Id: 3101, Type: 1, Name: "high-priority", Key: "sk-high", Status: common.ChannelStatusEnabled, Models: modelName, Group: "default", Priority: &highPriority},
		{Id: 3102, Type: 1, Name: "low-priority", Key: "sk-low", Status: common.ChannelStatusEnabled, Models: modelName, Group: "default", Priority: &lowPriority},
	} {
		require.NoError(t, model.DB.Create(ch).Error)
		require.NoError(t, model.DB.Create(&model.Ability{
			Group:     "default",
			Model:     modelName,
			ChannelId: ch.Id,
			Enabled:   true,
			Priority:  ch.Priority,
		}).Error)
	}
	require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 801, ModelId: modelName, ChannelId: 3102}).Error)

	model.InitChannelCache()
	model.InitTokenModelChannelCache()
	require.Eventually(t, func() bool {
		return model.IsChannelAllowedForToken(801, modelName, 3102) &&
			!model.IsChannelAllowedForToken(801, modelName, 3101)
	}, time.Second, 10*time.Millisecond)

	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")

	channel, group, err := CacheGetRandomSatisfiedChannel(&RetryParam{
		Ctx:        ctx,
		TokenGroup: "default",
		ModelName:  modelName,
		TokenId:    801,
		Retry:      common.GetPointer(0),
	})

	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, "default", group)
	assert.Equal(t, 3102, channel.Id)
}
