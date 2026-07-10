package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelModelHealthTestDB(t *testing.T) {
	t.Helper()
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = false
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelDisabled{}, &model.ChannelModelHealth{}))
	origDB := model.DB
	model.DB = db
	t.Cleanup(func() {
		model.DB = origDB
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
}

// TestDisableChannelModelFromRelayPreservesManualLock
// manual 人工锁存在时，relay 禁用不得覆盖 source——否则会被被动探活自动清除，人工意图被静默撤销
func TestDisableChannelModelFromRelayPreservesManualLock(t *testing.T) {
	setupChannelModelHealthTestDB(t)
	const cid, m = 31, "manual-locked"

	require.NoError(t, model.UpsertChannelModelDisabled(cid, m, model.DisabledSourceManual, "人工禁用"))

	DisableChannelModelFromRelay(cid, m, "401 invalid_api_key")

	var row model.ChannelModelDisabled
	require.NoError(t, model.DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, model.DisabledSourceManual, row.Source)
	assert.Equal(t, "人工禁用", row.Reason)
}

// TestDisableChannelModelFromRelayWritesRelaySource
// 无 manual 锁时保持既有语义：写 relay 禁用并把健康计数推到失败阈值
func TestDisableChannelModelFromRelayWritesRelaySource(t *testing.T) {
	setupChannelModelHealthTestDB(t)
	const cid, m = 32, "normal-model"

	DisableChannelModelFromRelay(cid, m, "500 upstream error")

	var row model.ChannelModelDisabled
	require.NoError(t, model.DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, model.DisabledSourceRelay, row.Source)

	h, err := model.GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, h.ConsecutiveFailures, 1)
}

// TestHandleConfirmedProbeResultReturnsErrorOnTxFailure
// 恢复事务失败必须向调用方返回 error（手测路径据此返回失败，避免假成功）
func TestHandleConfirmedProbeResultReturnsErrorOnTxFailure(t *testing.T) {
	setupChannelModelHealthTestDB(t)
	// 测试侧手段：删除表制造事务失败，不给生产代码加注入框架
	require.NoError(t, model.DB.Migrator().DropTable(&model.ChannelModelHealth{}))

	err := HandleConfirmedProbeResult(33, "any-model", true, "", 10, 1)
	assert.Error(t, err)
}

// TestHandleConfirmedProbeResultSuccessClearsAutoRelayKeepsManual
// 成功一次清 auto/relay、保留 manual（手测恢复与被动探活共用的核心语义）
func TestHandleConfirmedProbeResultSuccessClearsAutoRelayKeepsManual(t *testing.T) {
	setupChannelModelHealthTestDB(t)
	const cid = 34

	require.NoError(t, model.UpsertChannelModelDisabled(cid, "m-relay", model.DisabledSourceRelay, "relay fail"))
	require.NoError(t, model.UpsertChannelModelDisabled(cid, "m-manual", model.DisabledSourceManual, "人工禁用"))

	require.NoError(t, HandleConfirmedProbeResult(cid, "m-relay", true, "", 10, 1))
	require.NoError(t, HandleConfirmedProbeResult(cid, "m-manual", true, "", 10, 1))

	relayDisabled, err := model.IsChannelModelDisabled(cid, "m-relay")
	require.NoError(t, err)
	assert.False(t, relayDisabled, "relay 禁用应被一次成功清除")

	manualDisabled, err := model.IsChannelModelDisabled(cid, "m-manual")
	require.NoError(t, err)
	assert.True(t, manualDisabled, "manual 禁用不应被自动恢复清除")
}
