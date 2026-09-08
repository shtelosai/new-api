package model

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkModelRouteFiltersBeforePriorityAndFailover(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "数据库", true: "缓存"}[cached], func(t *testing.T) {
			cleanTokenChannelRoutingTables(t)
			previous := common.MemoryCacheEnabled
			common.MemoryCacheEnabled = cached
			t.Cleanup(func() { common.MemoryCacheEnabled = previous })
			const name = "kimi-k2.6"
			for i, entry := range []struct {
				kind     int
				wire     string
				priority int64
				granted  bool
			}{
				{14, "chat_completions", 100, true},
				{1, "responses", 90, true},
				{1, "chat_completions", 80, false},
				{1, "chat_completions", 20, true},
				{1, "chat_completions", 20, true},
				{1, "chat_completions", 10, true},
			} {
				id := 2801 + i
				seedTokenFilterChannel(t, id, name)
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{
					"type": entry.kind, "priority": entry.priority,
					"setting": `{"twork_runtime":"pi","twork_wire_api":"` + entry.wire + `"}`,
				}).Error)
				if entry.granted {
					require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 780, ModelId: name, ChannelId: id}).Error)
				}
			}
			InitChannelCache()
			refreshTokenModelChannelCache()
			policy := TworkRoutePolicy{ModelRoute: true, ExcludeAnthropic: true, WireAPI: "chat_completions"}
			excluded := map[int]struct{}{}
			for attempt := 0; attempt < 3; attempt++ {
				channel, err := GetTworkRoutedChannel(context.Background(), "default", name, 780, "/v1/chat/completions", policy, excluded, 2801)
				require.NoError(t, err)
				require.NotNil(t, channel)
				if attempt < 2 {
					assert.Contains(t, []int{2804, 2805}, channel.Id)
				} else {
					assert.Equal(t, 2806, channel.Id)
				}
				_, repeated := excluded[channel.Id]
				assert.False(t, repeated)
				excluded[channel.Id] = struct{}{}
			}
			channel, err := GetTworkRoutedChannel(context.Background(), "default", name, 780, "/v1/chat/completions", policy, excluded, 2804)
			require.NoError(t, err)
			assert.Nil(t, channel)
			// 持久化撤权后，即使授权缓存未刷新也必须立即拒绝。
			require.NoError(t, DB.Where("token_id = ?", 780).Delete(&TokenModelChannel{}).Error)
			channel, err = GetTworkRoutedChannel(context.Background(), "default", name, 780, "/v1/chat/completions", policy, nil, 2804)
			require.NoError(t, err)
			assert.Nil(t, channel)
		})
	}
}

func TestTworkLegacyPolicyPreservesClaudeAndBlocksNonClaudeAnthropic(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	for i, name := range []string{"kimi-k2.6", "vendor/claude-sonnet-4"} {
		seedTokenFilterChannel(t, 2901+i, name)
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2901+i).Update("type", 14).Error)
		channel, err := GetTworkRoutedChannel(context.Background(), "default", name, 0, "/v1/messages", TworkRoutePolicy{ExcludeAnthropic: true}, nil, 0)
		require.NoError(t, err)
		if i == 0 {
			assert.Nil(t, channel)
		} else {
			require.NotNil(t, channel)
			assert.Equal(t, 2902, channel.Id)
		}
	}
}

func TestTworkModelRouteCompactRespectsBillingModelDisable(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	seedTokenFilterChannel(t, 2950, "public-model")
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2950).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"responses"}`).Error)
	require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 790, ModelId: "public-model", ChannelId: 2950}).Error)
	require.NoError(t, DB.Create(&ChannelModelDisabled{ChannelId: 2950, Model: "public-model-compact", Source: "manual"}).Error)
	channel, err := GetTworkRoutedChannel(context.Background(), "default", "public-model", 790, "/v1/responses/compact", TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"}, nil, 0)
	require.NoError(t, err)
	assert.Nil(t, channel)
}

func TestTworkModelRouteAdvancesSharedMultiKeyPolling(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	previous := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = previous })
	seedTokenFilterChannel(t, 2960, "multi-key-model")
	info := ChannelInfo{IsMultiKey: true, MultiKeyMode: constant.MultiKeyModePolling, MultiKeySize: 2}
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2960).Updates(map[string]any{"key": "test-key-a\ntest-key-b", "channel_info": info, "setting": `{"twork_runtime":"pi","twork_wire_api":"responses"}`}).Error)
	require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 791, ModelId: "multi-key-model", ChannelId: 2960}).Error)
	InitChannelCache()
	for _, expected := range []string{"test-key-a", "test-key-b", "test-key-a"} {
		channel, err := GetTworkRoutedChannel(context.Background(), "default", "multi-key-model", 791, "/v1/responses", TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"}, nil, 0)
		require.NoError(t, err)
		require.NotNil(t, channel)
		key, _, apiErr := channel.GetNextEnabledKey()
		require.Nil(t, apiErr)
		assert.Equal(t, expected, key)
	}
}
