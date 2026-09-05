package model

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelRuntimeSettingsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		setting       string
		valid, legacy bool
	}{
		{"", true, true}, {`{}`, true, true}, {`{"twork_runtime":""}`, true, true}, {`{"twork_runtime":"legacy"}`, true, true},
		{`{"twork_runtime":"codex","TWORK_RUNTIME":"legacy"}`, false, false},
		{`{"twork_runtime":"legacy","twork_runtime":"codex"}`, false, false},
		{`{"TWORK_RUNTIME":"codex"}`, false, false},
		{`{"twork\u005fruntime":"codex"}`, false, false},
		{`{"twork_runtime":"codex"}`, true, false}, {`{"twork_runtime":"other"}`, false, false},
		{`{"twork_runtime":123}`, false, false}, {`{"twork_runtime":null}`, false, false}, {`{"twork_runtime":"codex",`, false, false}, {`null`, false, false}, {`[]`, false, false},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			ch := &Channel{Setting: common.GetPointer(tc.setting)}
			if tc.valid {
				assert.NoError(t, ch.ValidateSettings())
			} else {
				assert.Error(t, ch.ValidateSettings())
			}
			assert.Equal(t, tc.legacy, ch.AllowsLegacyRuntime())
			ch.GetSetting()
			require.NotNil(t, ch.Setting)
			assert.Equal(t, tc.setting, *ch.Setting)
		})
	}
}

func TestLegacySelectionExcludesDedicatedRuntimeBeforePriorityAndRetry(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	prevMemory := common.MemoryCacheEnabled
	t.Cleanup(func() { common.MemoryCacheEnabled = prevMemory })
	const name = "runtime-isolation-model"
	seedTokenFilterChannel(t, 2201, name)
	seedTokenFilterChannel(t, 2202, name)
	seedTokenFilterChannel(t, 2203, name)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2201).Updates(map[string]any{"setting": `{"twork_runtime":"codex"}`, "priority": 100}).Error)
	require.NoError(t, DB.Model(&Ability{}).Where("channel_id = ?", 2201).Update("priority", 100).Error)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2203).Update("setting", `{"twork_runtime":"codex",`).Error)
	for _, memory := range []bool{true, false} {
		common.MemoryCacheEnabled = memory
		InitChannelCache()
		for _, path := range []string{"/v1/responses", "/v1/messages", ""} {
			ch, err := GetRandomSatisfiedChannel("default", name, 0, path, 0)
			require.NoError(t, err)
			require.NotNil(t, ch)
			assert.Equal(t, 2202, ch.Id)
			ch, err = GetRandomSatisfiedChannelExcluding("default", name, 1, path, 0, map[int]struct{}{2202: {}})
			require.NoError(t, err)
			assert.Nil(t, ch)
		}
	}
}

func TestExplicitRuntimeAuthorizationReadsDatabaseOnEveryRequest(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	const name = "runtime-grant-model"
	seedTokenFilterChannel(t, 2301, name)
	seedTokenFilterChannel(t, 2302, name)
	for _, id := range []int{2301, 2302} {
		require.NoError(t, DB.Model(&Channel{}).Where("id = ?", id).Update("setting", `{"twork_runtime":"codex"}`).Error)
	}
	grant := TokenModelChannel{TokenId: 771, ModelId: name, ChannelId: 2302}
	require.NoError(t, DB.Create(&grant).Error)
	refreshTokenModelChannelCache()
	ctx := context.Background()
	ch, err := GetAuthorizedTworkChannel(ctx, 771, name, 2302, name)
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, 2302, ch.Id)
	for _, tc := range []struct {
		token, channel int
		model          string
	}{{772, 2302, name}, {771, 2301, name}, {771, 2302, "undeclared"}, {0, 2302, name}} {
		ch, err = GetAuthorizedTworkChannel(ctx, tc.token, tc.model, tc.channel, tc.model)
		assert.ErrorIs(t, err, ErrTworkRouteDenied)
		assert.Nil(t, ch)
	}
	require.NoError(t, DB.Delete(&grant).Error)
	// 缓存仍然含有旧授权，撤权必须立即拒绝。
	assert.True(t, IsChannelAllowedForToken(771, name, 2302))
	ch, err = GetAuthorizedTworkChannel(ctx, 771, name, 2302, name)
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
	assert.Nil(t, ch)
	require.NoError(t, DB.Create(&grant).Error)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2302).Update("models", "other").Error)
	ch, err = GetAuthorizedTworkChannel(ctx, 771, name, 2302, name)
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
	assert.Nil(t, ch)
}

func TestExplicitRuntimeAuthorizationRejectsDisabledCompactModel(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	const name = "runtime-compact-model"
	seedTokenFilterChannel(t, 2401, name)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2401).Update("setting", `{"twork_runtime":"codex"}`).Error)
	require.NoError(t, DB.Create(&TokenModelChannel{TokenId: 772, ModelId: name, ChannelId: 2401}).Error)
	disabled := ChannelModelDisabled{ChannelId: 2401, Model: name + "-openai-compact", Source: "manual"}
	require.NoError(t, DB.Create(&disabled).Error)
	t.Cleanup(func() { require.NoError(t, DB.Delete(&disabled).Error) })
	ch, err := GetAuthorizedTworkChannel(context.Background(), 772, name, 2401, name+"-openai-compact")
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
	assert.Nil(t, ch)
	ch, err = GetAuthorizedTworkChannel(context.Background(), 772, name, 2401, name)
	require.NoError(t, err)
	require.NotNil(t, ch)
}
