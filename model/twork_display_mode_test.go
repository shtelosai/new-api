package model

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkDisplayModeRejectsInvalidSettings(t *testing.T) {
	for _, setting := range []string{
		`{"twork_display_mode":"submenu"}`,
		`{"twork_runtime":"codex","twork_display_mode":"submenu"}`,
		`{"twork_runtime":"pi","twork_display_mode":"submenu"}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":""}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"Submenu"}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":null}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":true}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu","twork_display_mode":"default"}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","TWORK_DISPLAY_MODE":"submenu"}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_\u006dode":"submenu"}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu",`,
	} {
		t.Run(setting, func(t *testing.T) {
			ch := &Channel{Setting: common.GetPointer(setting)}
			assert.Error(t, ch.ValidateSettings())
			assert.False(t, ch.AllowsLegacyRuntime())
			assert.False(t, (TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"}).Allows(ch, "gpt-5"))
			ch.GetSetting()
			assert.Equal(t, setting, *ch.Setting)
		})
	}
}

func TestTworkDisplayModePreservesOldChannelsAndSettingsRoundTrip(t *testing.T) {
	for _, setting := range []string{`{}`, `{"twork_display_mode":"default"}`, `{"twork_runtime":"pi","twork_wire_api":"responses"}`, `{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"default"}`, `{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu"}`} {
		t.Run(setting, func(t *testing.T) {
			ch := &Channel{Setting: common.GetPointer(setting)}
			require.NoError(t, ch.ValidateSettings())
			ch.SetSetting(ch.GetSetting())
			var got map[string]any
			require.NoError(t, common.UnmarshalJsonStr(*ch.Setting, &got))
			var original map[string]any
			require.NoError(t, common.UnmarshalJsonStr(setting, &original))
			assert.Equal(t, original["twork_display_mode"], got["twork_display_mode"])
		})
	}
}

func TestTworkSubmenuExcludedFromModelPriorityAffinityAndRetry(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(map[bool]string{false: "数据库", true: "缓存"}[cached], func(t *testing.T) {
			cleanTokenChannelRoutingTables(t)
			previous := common.MemoryCacheEnabled
			common.MemoryCacheEnabled = cached
			t.Cleanup(func() { common.MemoryCacheEnabled = previous })
			const name = "submenu-model"
			for i, setting := range []string{
				`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu"}`,
				`{"twork_runtime":"pi","twork_wire_api":"responses"}`,
				`{}`,
			} {
				id := 2981 + i
				seedTokenFilterChannel(t, id, name)
				require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Updates(map[string]any{"setting": setting, "priority": 100 - i}).Error)
				require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", id).Update("priority", 100-i).Error)
				require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 798, ModelId: name, ChannelId: id}).Error)
			}
			InitChannelCache()
			refreshTokenModelChannelCache()
			policy := TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"}
			ch, err := GetTworkRoutedChannel(context.Background(), "default", name, 798, "/v1/responses", policy, nil, 2981)
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, 2982, ch.Id)
			ch, err = GetTworkRoutedChannel(context.Background(), "default", name, 798, "/v1/responses", policy, map[int]struct{}{2982: {}}, 2981)
			require.NoError(t, err)
			assert.Nil(t, ch)
			ch, err = GetRandomSatisfiedChannel("default", name, 0, "/v1/responses", 798)
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, 2983, ch.Id)
			ch, err = GetRandomSatisfiedChannelExcluding("default", name, 1, "/v1/responses", 798, map[int]struct{}{2983: {}})
			require.NoError(t, err)
			assert.Nil(t, ch)
			ch, err = GetAuthorizedTworkChannel(context.Background(), 798, name, 2981, name)
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, 2981, ch.Id)
			require.NoError(t, DB.Where("channel_id = ?", 2981).Delete(&TokenModelChannel{}).Error)
			ch, err = GetAuthorizedTworkChannel(context.Background(), 798, name, 2981, name)
			assert.ErrorIs(t, err, ErrTworkRouteDenied)
			assert.Nil(t, ch)
		})
	}
}

func TestChannelUpdatePreservesCMSManagedTworkSettings(t *testing.T) {
	for _, incoming := range []string{
		`{ "proxy":"http://127.0.0.1:3001" }`,
		`{"proxy":"http://127.0.0.1:3001","twork_runtime":"legacy","twork_display_mode":"default"}`,
		`{"proxy":"http://127.0.0.1:3001","twork_runtime":"pi","twork_wire_api":"chat_completions","twork_pi_compatibility":"bailian","twork_display_mode":"default"}`,
	} {
		t.Run(incoming, func(t *testing.T) {
			cleanTokenChannelRoutingTables(t)
			seedTokenFilterChannel(t, 2991, "cms-managed-model")
			const stored = `{"twork_runtime":"pi","twork_wire_api":"responses","twork_pi_compatibility":"openai","twork_display_mode":"submenu","proxy":"http://127.0.0.1:3000"}`
			require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2991).Update("setting", stored).Error)
			ch, err := GetChannelById(2991, true)
			require.NoError(t, err)
			ch.Setting = common.GetPointer(incoming)
			require.NoError(t, ch.Update())
			persisted, err := GetChannelById(2991, true)
			require.NoError(t, err)
			var got map[string]any
			require.NoError(t, common.UnmarshalJsonStr(*persisted.Setting, &got))
			assert.Equal(t, map[string]any{"twork_runtime": "pi", "twork_wire_api": "responses", "twork_pi_compatibility": "openai", "twork_display_mode": "submenu", "proxy": "http://127.0.0.1:3001"}, got)
		})
	}
}

func TestChannelUpdateRejectsDamagedCMSSettingWithoutOverwriting(t *testing.T) {
	for _, stored := range []string{
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu","TWORK_DISPLAY_MODE":"default"}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":null}`,
		`{"twork_runtime":"pi","twork_wire_api":"responses","twork_display_mode":"submenu",`,
	} {
		t.Run(stored, func(t *testing.T) {
			cleanTokenChannelRoutingTables(t)
			seedTokenFilterChannel(t, 2992, "damaged-submenu-model")
			require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2992).Update("setting", stored).Error)
			ch, err := GetChannelById(2992, true)
			require.NoError(t, err)
			ch.Setting = common.GetPointer(`{"proxy":"http://127.0.0.1:3001"}`)
			assert.Error(t, ch.Update())
			persisted, err := GetChannelById(2992, true)
			require.NoError(t, err)
			assert.Equal(t, stored, *persisted.Setting)
		})
	}
}
