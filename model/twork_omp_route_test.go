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

func TestOMPDerivesLegacyWireAndExcludesMenusAndUnknownTypes(t *testing.T) {
	for kind, wire := range map[int]string{14: "anthropic-messages", 1: "chat-completions"} {
		ch := &Channel{Type: kind}
		p := TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: wire}
		assert.True(t, p.Allows(ch, "any-name"))
		assert.True(t, p.Allows(ch, "auto"))
		ch.Type = 999
		assert.False(t, p.Allows(ch, "any-name"))
	}
	ch := &Channel{Setting: common.GetPointer(`{"twork_runtime":"omp","twork_wire_api":"responses","twork_display_mode":"submenu"}`)}
	assert.False(t, (TworkRoutePolicy{OMPVersion: "18.10.1", ModelRoute: true, WireAPI: "responses"}).Allows(ch, "x"))
	assert.False(t, ch.AllowsLegacyRuntime())
}
