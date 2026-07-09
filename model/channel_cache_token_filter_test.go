package model

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func cleanTokenChannelRoutingTables(t *testing.T) {
	t.Helper()
	for _, table := range []string{"token_model_channels", "channels", "abilities"} {
		require.NoError(t, DB.Exec("DELETE FROM "+table).Error)
	}
	empty := make(map[string][]int)
	tokenModelChannelCachePtr.Store(&empty)
	t.Cleanup(func() {
		for _, table := range []string{"token_model_channels", "channels", "abilities"} {
			require.NoError(t, DB.Exec("DELETE FROM "+table).Error)
		}
		tokenModelChannelCachePtr.Store(&empty)
	})
}

func seedTokenFilterChannel(t *testing.T, id int, modelName string) {
	t.Helper()
	priority := int64(0)
	require.NoError(t, DB.Create(&Channel{
		Id:       id,
		Type:     1,
		Name:     "token-filter-channel",
		Key:      "sk-test",
		Status:   common.ChannelStatusEnabled,
		Models:   modelName,
		Group:    "default",
		Priority: &priority,
	}).Error)
	require.NoError(t, DB.Create(&Ability{
		Group:     "default",
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
	}).Error)
}

func TestGetRandomSatisfiedChannelFiltersByTokenModelWhitelist(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	prevMemory := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = prevMemory })

	modelName := "f2-token-filter-model"
	for _, id := range []int{2101, 2102, 2103} {
		seedTokenFilterChannel(t, id, modelName)
	}
	require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 501, ModelId: modelName, ChannelId: 2102}).Error)
	require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 503, ModelId: modelName, ChannelId: 2999}).Error)

	InitChannelCache()
	refreshTokenModelChannelCache()

	channel, err := GetRandomSatisfiedChannel("default", modelName, 0, "/v1/responses", 501)
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Equal(t, 2102, channel.Id)
	assert.True(t, IsChannelAllowedForToken(501, modelName, 2102))
	assert.False(t, IsChannelAllowedForToken(501, modelName, 2101))

	channel, err = GetRandomSatisfiedChannel("default", modelName, 0, "/v1/responses", 503)
	require.NoError(t, err)
	assert.Nil(t, channel)

	channel, err = GetRandomSatisfiedChannel("default", modelName, 0, "/v1/responses", 502)
	require.NoError(t, err)
	require.NotNil(t, channel)
	assert.Contains(t, []int{2101, 2102, 2103}, channel.Id)
	assert.True(t, IsChannelAllowedForToken(502, modelName, 2101))
}
