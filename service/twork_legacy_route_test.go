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

func TestTworkLegacyPolicyKeepsCachedRetrySelection(t *testing.T) {
	cleanServiceTokenRoutingTables(t)
	previous := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previous })
	const name = "legacy-qwen"
	seedRetryChannel(t, 3301, name, 100)
	seedRetryChannel(t, 3302, name, 20)
	seedRetryChannel(t, 3303, name, 10)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 3301).Update("type", constant.ChannelTypeAnthropic).Error)
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	common.SetContextKey(c, constant.ContextKeyTworkRoutePolicy, model.TworkRoutePolicy{ExcludeAnthropic: true})
	param := &RetryParam{Ctx: c, ModelName: name, RequestPath: "/v1/messages"}
	// 缓存已加载后关闭数据库句柄，旧协议选路仍应完全使用缓存。
	previousDB := model.DB
	model.DB = nil
	defer func() { model.DB = previousDB }()
	for _, expected := range []int{3302, 3303} {
		channel, err := param.selectChannel("default", 0, param.ExcludedChannelIds)
		require.NoError(t, err)
		require.NotNil(t, channel)
		assert.Equal(t, expected, channel.Id)
		param.ExcludeChannel(channel.Id)
	}
}
