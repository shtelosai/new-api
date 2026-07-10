package model

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUpsertPreservingManual_KeepsManualLock
// 已有 manual 人工锁时，relay 禁用写入不得覆盖（否则随后会被探活自动清除，人工意图被静默撤销）
func TestUpsertPreservingManual_KeepsManualLock(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 21, "manual-locked-model"

	require.NoError(t, UpsertChannelModelDisabled(cid, m, DisabledSourceManual, "人工禁用"))

	changed, err := UpsertChannelModelDisabledPreservingManual(cid, m, DisabledSourceRelay, "relay fail")
	require.NoError(t, err)
	assert.False(t, changed, "manual 锁不应被 relay 覆盖")

	var row ChannelModelDisabled
	require.NoError(t, DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, DisabledSourceManual, row.Source)
	assert.Equal(t, "人工禁用", row.Reason)
}

// TestUpsertPreservingManual_OverwritesAutoAndCreates
// auto/relay 行照常覆盖；无行时新建
func TestUpsertPreservingManual_OverwritesAutoAndCreates(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 22, "relay-model"

	// 无行 → 新建
	changed, err := UpsertChannelModelDisabledPreservingManual(cid, m, DisabledSourceRelay, "first fail")
	require.NoError(t, err)
	assert.True(t, changed)

	// auto 行 → 覆盖为 relay
	require.NoError(t, UpsertChannelModelDisabled(cid, m, DisabledSourceAuto, "probe fail"))
	changed, err = UpsertChannelModelDisabledPreservingManual(cid, m, DisabledSourceRelay, "relay fail again")
	require.NoError(t, err)
	assert.True(t, changed)

	var row ChannelModelDisabled
	require.NoError(t, DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, DisabledSourceRelay, row.Source)
	assert.Equal(t, "relay fail again", row.Reason)

	// 只有一行
	var count int64
	DB.Model(&ChannelModelDisabled{}).Where("channel_id = ? AND model = ?", cid, m).Count(&count)
	assert.Equal(t, int64(1), count)
}

// TestClearChannelModelDisabled_ClearsAnySourceAndResetsHealth
// 显式「解除禁用」删除任意 source（含 manual）并重置健康失败状态
func TestClearChannelModelDisabled_ClearsAnySourceAndResetsHealth(t *testing.T) {
	cleanupHealthTables(t)
	const cid = 23

	for _, tc := range []struct {
		model  string
		source string
	}{
		{"m-auto", DisabledSourceAuto},
		{"m-relay", DisabledSourceRelay},
		{"m-manual", DisabledSourceManual},
	} {
		require.NoError(t, UpsertChannelModelDisabled(cid, tc.model, tc.source, "reason"))
		require.NoError(t, DB.Create(&ChannelModelHealth{
			ChannelId:           cid,
			Model:               tc.model,
			ConsecutiveFailures: 3,
			LastError:           "old error",
		}).Error)

		previousSource, changed, err := ClearChannelModelDisabledInTx(cid, tc.model)
		require.NoError(t, err)
		assert.True(t, changed)
		assert.Equal(t, tc.source, previousSource)

		disabled, err := IsChannelModelDisabled(cid, tc.model)
		require.NoError(t, err)
		assert.False(t, disabled)

		h, err := GetChannelModelHealth(cid, tc.model)
		require.NoError(t, err)
		assert.Equal(t, 0, h.ConsecutiveFailures)
		assert.Equal(t, "", h.LastError)
	}
}

// TestClearChannelModelDisabled_IdempotentWhenAbsent
// 行不存在时幂等成功（changed=false）
func TestClearChannelModelDisabled_IdempotentWhenAbsent(t *testing.T) {
	cleanupHealthTables(t)

	previousSource, changed, err := ClearChannelModelDisabledInTx(24, "no-such-model")
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, "", previousSource)
}
