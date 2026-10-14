package model

import (
	"context"
	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestOMPRoutesByProtocolAndLiveGrantsWithoutModelNameHeuristics(t *testing.T) {
	for _, cached := range []bool{false, true} {
		for wire, kind := range map[string]int{"anthropic-messages": 14, "responses": 1, "chat-completions": 1} {
			t.Run(wire+map[bool]string{true: "-cache", false: "-db"}[cached], func(t *testing.T) {
				cleanTokenChannelRoutingTables(t)
				previous := common.MemoryCacheEnabled
				common.MemoryCacheEnabled = cached
				t.Cleanup(func() { common.MemoryCacheEnabled = previous })
				const name = "arbitrary-alias"
				for i := 0; i < 3; i++ {
					id := 3100 + i
					seedTokenFilterChannel(t, id, name)
					require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{"type": kind, "setting": `{"twork_runtime":"omp","twork_wire_api":"` + wire + `"}`, "priority": 100 - i}).Error)
					if i > 0 {
						require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 900, ModelId: name, ChannelId: id}).Error)
					}
				}
				InitChannelCache()
				refreshTokenModelChannelCache()
				policy := TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: wire}
				ch, err := GetTworkRoutedChannel(context.Background(), "default", name, 900, "", policy, nil, 3100)
				require.NoError(t, err)
				require.NotNil(t, ch)
				assert.Equal(t, 3101, ch.Id)
				ch, err = GetTworkRoutedChannel(context.Background(), "default", name, 900, "", policy, map[int]struct{}{3101: {}}, 3101)
				require.NoError(t, err)
				require.NotNil(t, ch)
				assert.Equal(t, 3102, ch.Id)
				_, err = GetAuthorizedTworkChannel(context.Background(), 900, name, 3100, name, true)
				assert.ErrorIs(t, err, ErrTworkRouteDenied)
				_, err = GetAuthorizedTworkChannel(context.Background(), 900, name, 3101, name, true)
				assert.NoError(t, err)
				require.NoError(t, DB.Where("token_id = ?", 900).Delete(&TokenModelChannel{}).Error)
				ch, err = GetTworkRoutedChannel(context.Background(), "default", name, 900, "", policy, nil, 3101)
				require.NoError(t, err)
				assert.Nil(t, ch)
				_, err = GetAuthorizedTworkChannel(context.Background(), 900, name, 3101, name, true)
				assert.ErrorIs(t, err, ErrTworkRouteDenied)
			})
		}
	}
}

func TestOMPUsesOnlyOMPChannelsAndOldRoutesNeverUseThem(t *testing.T) {
	old := map[string]*Channel{
		"legacy-anthropic": {Type: 14},
		"legacy-openai":    {Type: 1},
		"pi-chat":          {Type: 1, Setting: common.GetPointer(`{"twork_runtime":"pi","twork_wire_api":"chat_completions"}`)},
		"pi-responses":     {Type: 1, Setting: common.GetPointer(`{"twork_runtime":"pi","twork_wire_api":"responses"}`)},
		"codex":            {Type: 1, Setting: common.GetPointer(`{"twork_runtime":"codex"}`)},
	}
	for name, ch := range old {
		for _, wire := range []string{"anthropic-messages", "chat-completions", "responses"} {
			assert.False(t, (TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: wire}).Allows(ch, "any-name"), name+" 不能进入 OMP "+wire)
		}
		wire, err := ch.TworkOMPWireAPI()
		require.NoError(t, err)
		assert.Empty(t, wire, name)
	}
	ompChannel := &Channel{Type: 1, Setting: common.GetPointer(`{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`)}
	assert.True(t, (TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: "chat-completions"}).Allows(ompChannel, "any-name"))
	assert.False(t, (TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: "responses"}).Allows(ompChannel, "any-name"))
	// 旧客户端的自然分配与 Pi 模型级选路都不能选中 OMP 渠道。
	assert.False(t, ompChannel.AllowsLegacyRuntime())
	assert.False(t, (TworkRoutePolicy{}).Allows(ompChannel, "any-name"))
	assert.False(t, (TworkRoutePolicy{ModelRoute: true, ExcludeAnthropic: true, WireAPI: "chat_completions"}).Allows(ompChannel, "any-name"))
	menu := &Channel{Setting: common.GetPointer(`{"twork_runtime":"omp","twork_wire_api":"responses","twork_display_mode":"submenu"}`)}
	assert.False(t, (TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: "responses"}).Allows(menu, "x"))
}

func TestOMPPinnedRouteRejectsOldClientChannelsAndOldPinnedRouteRejectsOMP(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	const name = "pinned-alias"
	settings := map[int]string{
		3200: `{"twork_runtime":"omp","twork_wire_api":"responses"}`,
		3201: `{"twork_runtime":"pi","twork_wire_api":"responses"}`,
		3202: `{"twork_runtime":"codex"}`,
		3203: `{}`,
	}
	for id, setting := range settings {
		seedTokenFilterChannel(t, id, name)
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{"type": 1, "setting": setting}).Error)
		require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 901, ModelId: name, ChannelId: id}).Error)
	}
	_, err := GetAuthorizedTworkChannel(context.Background(), 901, name, 3200, name, true)
	assert.NoError(t, err)
	for _, id := range []int{3201, 3202, 3203} {
		_, err = GetAuthorizedTworkChannel(context.Background(), 901, name, id, name, true)
		assert.ErrorIs(t, err, ErrTworkRouteDenied, "OMP 不能固定到旧渠道 %d", id)
	}
	_, err = GetAuthorizedTworkChannel(context.Background(), 901, name, 3200, name)
	assert.ErrorIs(t, err, ErrTworkRouteDenied, "旧客户端不能固定到 OMP 渠道")
	_, err = GetAuthorizedTworkChannel(context.Background(), 901, name, 3201, name)
	assert.NoError(t, err)
}
