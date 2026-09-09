package model

import (
	"context"
	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestTworkProtocolBoundaryIncludesAliasesAndPreservesLegacy(t *testing.T) {
	for _, name := range []string{"auto", "summarization-model", "qwen3.8-flash", "glm-5.3-flash", "claude-sonnet-5", "vendor/Claude-Opus-5"} {
		for _, kind := range []int{1, 14, 24} {
			channel := &Channel{Type: kind}
			claude := name == "claude-sonnet-5" || name == "vendor/Claude-Opus-5"
			require.Equal(t, claude == (kind == 14), (TworkRoutePolicy{ExcludeAnthropic: true}).Allows(channel, name), "%s type=%d", name, kind)
			require.True(t, (TworkRoutePolicy{}).Allows(channel, name), "旧版保持原路由")
		}
	}
}

func TestTworkAliasFilterBeforePriorityAffinityAndRetry(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for _, name := range []string{"auto", "summarization-model"} {
			t.Run(name+map[bool]string{true: "缓存", false: "数据库"}[cached], func(t *testing.T) {
				cleanTokenChannelRoutingTables(t)
				old := common.MemoryCacheEnabled
				common.MemoryCacheEnabled = cached
				t.Cleanup(func() { common.MemoryCacheEnabled = old })
				for i, kind := range []int{14, 1, 24} {
					id := 3101 + i
					seedTokenFilterChannel(t, id, name)
					require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{"type": kind, "priority": 100 - i*10}).Error)
				}
				InitChannelCache()
				policy := TworkRoutePolicy{ExcludeAnthropic: true}
				excluded := map[int]struct{}{}
				for _, want := range []int{3102, 3103} {
					ch, err := GetTworkRoutedChannel(context.Background(), "default", name, 0, "/v1/messages", policy, excluded, 3101)
					require.NoError(t, err)
					require.NotNil(t, ch)
					require.Equal(t, want, ch.Id)
					excluded[ch.Id] = struct{}{}
				}
				ch, err := GetTworkRoutedChannel(context.Background(), "default", name, 0, "/v1/messages", policy, excluded, 3101)
				require.NoError(t, err)
				require.Nil(t, ch, "重试耗尽不得回到 Anthropic")
			})
		}
	}
}
