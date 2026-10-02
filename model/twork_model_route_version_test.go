package model

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkModelRouteVersionKeepsAuthorizationAndDisableBoundaries(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	const name = "pi-version-model"
	const tokenID = 799
	for i, tc := range []struct {
		profile, display string
		granted, enabled bool
	}{
		{"unknown-profile", "", true, true},
		{"meta", "", false, true},
		{"meta", "", true, false},
		{"meta", "", true, true},
		{"meta", "", true, true},
		{"meta", "submenu", true, true},
		{"", "", true, true},
	} {
		id := 2990 + i
		seedTokenFilterChannel(t, id, name)
		setting := `{"twork_runtime":"pi","twork_wire_api":"responses"`
		if tc.profile != "" {
			setting += `,"twork_pi_compatibility":"` + tc.profile + `"`
		}
		if tc.display != "" {
			setting += `,"twork_display_mode":"` + tc.display + `"`
		}
		setting += `}`
		status := common.ChannelStatusEnabled
		if !tc.enabled {
			status = common.ChannelStatusManuallyDisabled
		}
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{
			"setting": setting, "priority": 100 - i, "status": status,
		}).Error)
		if tc.granted {
			require.NoError(t, DB.Create(&TokenModelChannel{TokenId: tokenID, ModelId: name, ChannelId: id}).Error)
		}
	}
	require.NoError(t, DB.Create(&ChannelModelDisabled{ChannelId: 2993, Model: name, Source: "manual"}).Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Where("channel_id = ? AND model = ?", 2993, name).Delete(&ChannelModelDisabled{}).Error)
	})
	InitChannelCache()
	refreshTokenModelChannelCache()

	for _, version := range []string{"", "0.85.1", "1.0.0", "1.1.0"} {
		t.Run(version, func(t *testing.T) {
			policy := TworkRoutePolicy{ModelRoute: true, ExcludeAnthropic: true, WireAPI: "responses", PiCompatibilityVersion: version}
			channel, err := GetTworkRoutedChannel(context.Background(), "default", name, tokenID, "/v1/responses", policy, nil, 2991)
			require.NoError(t, err)
			require.NotNil(t, channel)
			expected := 2996
			if version == "1.0.0" {
				expected = 2994
			}
			assert.Equal(t, expected, channel.Id)
			channel, err = GetTworkRoutedChannel(context.Background(), "default", name, tokenID, "/v1/responses", policy, map[int]struct{}{2994: {}}, 2994)
			require.NoError(t, err)
			require.NotNil(t, channel)
			assert.Equal(t, 2996, channel.Id)
			channel, err = GetTworkRoutedChannel(context.Background(), "default", name, tokenID+1, "/v1/responses", policy, nil, 2994)
			require.NoError(t, err)
			assert.Nil(t, channel)
		})
	}
	// 持久化撤权立即生效，不受新版兼容声明或旧授权缓存影响。
	require.NoError(t, DB.Where("token_id = ?", tokenID).Delete(&TokenModelChannel{}).Error)
	for _, version := range []string{"0.85.1", "1.0.0"} {
		channel, err := GetTworkRoutedChannel(context.Background(), "default", name, tokenID, "/v1/responses", TworkRoutePolicy{ModelRoute: true, WireAPI: "responses", PiCompatibilityVersion: version}, nil, 2994)
		require.NoError(t, err)
		assert.Nil(t, channel)
	}
}
