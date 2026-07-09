package model

import (
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// cleanupHealthTables 清理两张新表（每个测试开头调用保证隔离）
func cleanupHealthTables(t *testing.T) {
	t.Helper()
	require.NoError(t, DB.AutoMigrate(&ChannelModelDisabled{}, &ChannelModelHealth{}))
	require.NoError(t, DB.Exec("DELETE FROM channel_model_disabled").Error)
	require.NoError(t, DB.Exec("DELETE FROM channel_model_health").Error)
}

func TestAttachChannelModelStatusesBuildsDisabledHealthyUnknown(t *testing.T) {
	cleanupHealthTables(t)
	const cid = 901
	now := time.Now().Unix()

	require.NoError(t, DB.Create(&ChannelModelDisabled{
		ChannelId: cid,
		Model:     "model-b",
		Source:    DisabledSourceAuto,
		Reason:    "probe failed",
	}).Error)
	require.NoError(t, DB.Create(&ChannelModelHealth{
		ChannelId:           cid,
		Model:               "model-a",
		ConsecutiveFailures: 0,
		LastSuccessAt:       now,
	}).Error)
	require.NoError(t, DB.Create(&ChannelModelHealth{
		ChannelId:           cid,
		Model:               "model-c",
		ConsecutiveFailures: 1,
		LastSuccessAt:       now,
		LastError:           "last failure",
	}).Error)

	channels := []*Channel{{
		Id:     cid,
		Models: "model-a, model-b,model-c,model-d",
	}}
	require.NoError(t, AttachChannelModelStatuses(channels))

	require.Len(t, channels[0].ModelStatuses, 4)
	assert.Equal(t, ChannelModelStatus{Model: "model-a", Status: ChannelModelStatusHealthy}, channels[0].ModelStatuses[0])
	assert.Equal(t, ChannelModelStatus{
		Model:  "model-b",
		Status: ChannelModelStatusDisabled,
		Source: DisabledSourceAuto,
		Reason: "probe failed",
	}, channels[0].ModelStatuses[1])
	assert.Equal(t, ChannelModelStatus{Model: "model-c", Status: ChannelModelStatusUnknown}, channels[0].ModelStatuses[2])
	assert.Equal(t, ChannelModelStatus{Model: "model-d", Status: ChannelModelStatusUnknown}, channels[0].ModelStatuses[3])
}

func TestAttachChannelModelStatusesKeepsSameModelPerChannelSeparate(t *testing.T) {
	cleanupHealthTables(t)
	now := time.Now().Unix()

	require.NoError(t, DB.Create(&ChannelModelDisabled{
		ChannelId: 902,
		Model:     "shared-model",
		Source:    DisabledSourceRelay,
		Reason:    "relay failed",
	}).Error)
	require.NoError(t, DB.Create(&ChannelModelHealth{
		ChannelId:           903,
		Model:               "shared-model",
		ConsecutiveFailures: 0,
		LastSuccessAt:       now,
	}).Error)

	channels := []*Channel{
		{Id: 902, Models: "shared-model"},
		{Id: 903, Models: "shared-model"},
	}
	require.NoError(t, AttachChannelModelStatuses(channels))

	require.Len(t, channels[0].ModelStatuses, 1)
	require.Len(t, channels[1].ModelStatuses, 1)
	assert.Equal(t, ChannelModelStatusDisabled, channels[0].ModelStatuses[0].Status)
	assert.Equal(t, ChannelModelStatusHealthy, channels[1].ModelStatuses[0].Status)
}

// TestApplyTestResult_DisableAfterConsecutiveFailures
// 健康 → 连续 3 次失败 → 写 disabled(source=auto)
func TestApplyTestResult_DisableAfterConsecutiveFailures(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 1, "gpt-4o"
	const failN, successM = 3, 2

	// 第 1 次失败：计数=1，不触发
	action, err := ApplyTestResultInTx(cid, m, false, "err-1", 100, failN, successM)
	require.NoError(t, err)
	assert.Equal(t, HealthActionNone, action)
	disabled, err := IsChannelModelDisabled(cid, m)
	require.NoError(t, err)
	assert.False(t, disabled)

	// 第 2 次失败：计数=2，不触发
	action, err = ApplyTestResultInTx(cid, m, false, "err-2", 200, failN, successM)
	require.NoError(t, err)
	assert.Equal(t, HealthActionNone, action)
	disabled, _ = IsChannelModelDisabled(cid, m)
	assert.False(t, disabled)

	// 第 3 次失败：计数=3，触发禁用
	action, err = ApplyTestResultInTx(cid, m, false, "err-3", 300, failN, successM)
	require.NoError(t, err)
	assert.Equal(t, HealthActionDisabled, action)
	disabled, _ = IsChannelModelDisabled(cid, m)
	assert.True(t, disabled)

	// 验证 disabled 行的 source=auto 和 reason
	var row ChannelModelDisabled
	require.NoError(t, DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, DisabledSourceAuto, row.Source)
	assert.Equal(t, "err-3", row.Reason)

	// 验证 health 状态
	h, err := GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.Equal(t, 3, h.ConsecutiveFailures)
	assert.Equal(t, 0, h.ConsecutiveSuccesses)
	assert.Equal(t, 300, h.LatencyMs)
	assert.Equal(t, "err-3", h.LastError)
}

// TestApplyTestResult_RecoverAfterConsecutiveSuccesses
// disabled(auto) → 连续 2 次成功 → 自动删除 disabled 行
func TestApplyTestResult_RecoverAfterConsecutiveSuccesses(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 2, "claude-sonnet"
	const failN, successM = 3, 2

	// 先达阈值触发 auto 禁用
	for i := 0; i < failN; i++ {
		_, err := ApplyTestResultInTx(cid, m, false, "fail", 100, failN, successM)
		require.NoError(t, err)
	}
	disabled, _ := IsChannelModelDisabled(cid, m)
	require.True(t, disabled)

	// 第 1 次成功：计数=1，不触发恢复
	action, err := ApplyTestResultInTx(cid, m, true, "", 50, failN, successM)
	require.NoError(t, err)
	assert.Equal(t, HealthActionNone, action)
	disabled, _ = IsChannelModelDisabled(cid, m)
	assert.True(t, disabled)

	// 第 2 次成功：计数=2，触发恢复
	action, err = ApplyTestResultInTx(cid, m, true, "", 60, failN, successM)
	require.NoError(t, err)
	assert.Equal(t, HealthActionRecovered, action)
	disabled, _ = IsChannelModelDisabled(cid, m)
	assert.False(t, disabled)

	h, err := GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.Equal(t, 0, h.ConsecutiveFailures)
	assert.Equal(t, 2, h.ConsecutiveSuccesses)
	assert.Equal(t, "", h.LastError)
}

// TestApplyConfirmedProbeResult_DisableAndRecover
// 同一轮内已确认失败后立即禁用；成功一次立即恢复 auto/relay 禁用
func TestApplyConfirmedProbeResult_DisableAndRecover(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 11, "confirmed-model"

	action, err := ApplyConfirmedProbeResultInTx(cid, m, false, "confirmed failure", 20000, 3)
	require.NoError(t, err)
	assert.Equal(t, HealthActionDisabled, action)

	disabled, err := IsChannelModelDisabled(cid, m)
	require.NoError(t, err)
	assert.True(t, disabled)

	h, err := GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.Equal(t, 3, h.ConsecutiveFailures)
	assert.Equal(t, 0, h.ConsecutiveSuccesses)
	assert.Equal(t, "confirmed failure", h.LastError)

	action, err = ApplyConfirmedProbeResultInTx(cid, m, true, "", 50, 0)
	require.NoError(t, err)
	assert.Equal(t, HealthActionRecovered, action)

	disabled, err = IsChannelModelDisabled(cid, m)
	require.NoError(t, err)
	assert.False(t, disabled)

	h, err = GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.Equal(t, 0, h.ConsecutiveFailures)
	assert.Equal(t, 1, h.ConsecutiveSuccesses)
	assert.Equal(t, "", h.LastError)
}

// TestApplyTestResult_BelowThresholdNoDisable
// 2 次失败未达阈值不触发禁用
func TestApplyTestResult_BelowThresholdNoDisable(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 3, "gemini-pro"
	const failN, successM = 3, 2

	for i := 0; i < 2; i++ {
		action, err := ApplyTestResultInTx(cid, m, false, "err", 100, failN, successM)
		require.NoError(t, err)
		assert.Equal(t, HealthActionNone, action)
	}
	disabled, _ := IsChannelModelDisabled(cid, m)
	assert.False(t, disabled)
}

// TestUpsertChannelModelDisabled_Relay
// Relay 失败 Upsert 写 source=relay（模拟 DisableChannelModelFromRelay 的一半工作）
func TestUpsertChannelModelDisabled_Relay(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 4, "gpt-4o-mini"

	require.NoError(t, UpsertChannelModelDisabled(cid, m, DisabledSourceRelay, "401 invalid_api_key"))
	disabled, _ := IsChannelModelDisabled(cid, m)
	assert.True(t, disabled)

	var row ChannelModelDisabled
	require.NoError(t, DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, DisabledSourceRelay, row.Source)
}

// TestApplyTestResult_ManualDisabledNotAutoRecovered
// manual 禁用的行不被健康检查自动恢复流程清除
func TestApplyTestResult_ManualDisabledNotAutoRecovered(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 5, "o1-mini"
	const failN, successM = 3, 2

	require.NoError(t, UpsertChannelModelDisabled(cid, m, DisabledSourceManual, "人工禁用"))

	// 连续 N 次成功（超过 successM）
	for i := 0; i < successM+2; i++ {
		action, err := ApplyTestResultInTx(cid, m, true, "", 50, failN, successM)
		require.NoError(t, err)
		// 期望 Action=None，因为 manual 不会被 DELETE WHERE source IN (auto, relay) 清掉
		assert.Equal(t, HealthActionNone, action)
	}

	disabled, _ := IsChannelModelDisabled(cid, m)
	assert.True(t, disabled, "manual 禁用应被保留")

	// 确认 source 仍是 manual
	var row ChannelModelDisabled
	require.NoError(t, DB.Where("channel_id = ? AND model = ?", cid, m).First(&row).Error)
	assert.Equal(t, DisabledSourceManual, row.Source)
}

// TestApplyTestResult_RelayOverwrittenByAutoWithOnlyOneRow
// 已存在 relay 禁用时，再次达阈值不会新增行（只保留一条）
func TestApplyTestResult_NoDuplicateOnReDisable(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 6, "sonnet"
	const failN, successM = 3, 2

	// 先 relay 禁用
	require.NoError(t, UpsertChannelModelDisabled(cid, m, DisabledSourceRelay, "relay fail"))

	// 健康检查连续 3 次失败
	for i := 0; i < failN; i++ {
		action, err := ApplyTestResultInTx(cid, m, false, "fail", 100, failN, successM)
		require.NoError(t, err)
		// 第 3 次达阈值，但因为已有 relay 禁用行，count>0，不会新增，action=None
		if i == failN-1 {
			assert.Equal(t, HealthActionNone, action)
		}
	}

	// 只应有 1 条禁用记录
	var count int64
	DB.Model(&ChannelModelDisabled{}).Where("channel_id = ? AND model = ?", cid, m).Count(&count)
	assert.Equal(t, int64(1), count)
}

// TestUpdateHealthObservability_NoCountChange
// 观测字段更新不应改变计数（localErr 场景）
func TestUpdateHealthObservability_NoCountChange(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 7, "e5-small"

	// 先正常累积 1 次失败计数
	_, err := ApplyTestResultInTx(cid, m, false, "real-fail", 100, 3, 2)
	require.NoError(t, err)

	// 观测更新
	require.NoError(t, UpdateHealthObservability(cid, m, "local: unsupported", 50))

	// 计数不应变化
	h, err := GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.Equal(t, 1, h.ConsecutiveFailures)
	// last_error / latency_ms 被观测更新覆盖
	assert.Equal(t, "local: unsupported", h.LastError)
	assert.Equal(t, 50, h.LatencyMs)
}

// TestApplyTestResult_ConcurrentWrites
// 并发场景验证事务隔离：10 goroutine 同时 ApplyTestResultInTx 都失败，
// 累计计数应 == 10（无丢失更新），仅产生 1 条 disabled 行
func TestApplyTestResult_ConcurrentWrites(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m = 8, "concurrent-model"
	const failN, successM = 3, 2
	const goroutines = 10

	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ApplyTestResultInTx(cid, m, false, "race", 100, failN, successM)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	h, err := GetChannelModelHealth(cid, m)
	require.NoError(t, err)
	assert.Equal(t, goroutines, h.ConsecutiveFailures, "并发计数必须无丢失")

	// 只应有 1 条 disabled 记录（触发即插入，后续 count>0 不再新增）
	var count int64
	DB.Model(&ChannelModelDisabled{}).Where("channel_id = ? AND model = ?", cid, m).Count(&count)
	assert.Equal(t, int64(1), count)
}

// TestDeleteByChannel_CleansBothTables
// 脏数据清理钩子：按 channel 删除两张表
func TestDeleteByChannel_CleansBothTables(t *testing.T) {
	cleanupHealthTables(t)
	const cid, m1, m2 = 9, "a", "b"

	require.NoError(t, UpsertChannelModelDisabled(cid, m1, DisabledSourceAuto, "r"))
	require.NoError(t, UpsertChannelModelDisabled(cid, m2, DisabledSourceRelay, "r"))
	_, _ = ApplyTestResultInTx(cid, m1, false, "r", 1, 3, 2)
	_, _ = ApplyTestResultInTx(cid, m2, false, "r", 1, 3, 2)

	require.NoError(t, DeleteChannelModelDisabledByChannel(cid))
	require.NoError(t, DeleteHealthByChannel(cid))

	var dc, hc int64
	DB.Model(&ChannelModelDisabled{}).Where("channel_id = ?", cid).Count(&dc)
	DB.Model(&ChannelModelHealth{}).Where("channel_id = ?", cid).Count(&hc)
	assert.Equal(t, int64(0), dc)
	assert.Equal(t, int64(0), hc)
}

// TestDeleteByChannelAndModels_KeepsInList
// 脏数据清理钩子：按 channel + keepModels 白名单清理
func TestDeleteByChannelAndModels_KeepsInList(t *testing.T) {
	cleanupHealthTables(t)
	const cid = 10

	for _, m := range []string{"keep-1", "keep-2", "remove-1", "remove-2"} {
		require.NoError(t, UpsertChannelModelDisabled(cid, m, DisabledSourceAuto, "r"))
		_, _ = ApplyTestResultInTx(cid, m, false, "r", 1, 3, 2)
	}

	keep := []string{"keep-1", "keep-2"}
	require.NoError(t, DeleteChannelModelDisabledNotInModels(cid, keep))
	require.NoError(t, DeleteHealthByChannelNotInModels(cid, keep))

	var dc, hc int64
	DB.Model(&ChannelModelDisabled{}).Where("channel_id = ?", cid).Count(&dc)
	DB.Model(&ChannelModelHealth{}).Where("channel_id = ?", cid).Count(&hc)
	assert.Equal(t, int64(2), dc)
	assert.Equal(t, int64(2), hc)

	// keep 的行应仍存在
	for _, km := range keep {
		disabled, _ := IsChannelModelDisabled(cid, km)
		assert.True(t, disabled, "keep="+km+" 应保留")
	}
}

// TestInitChannelCache_FiltersDisabled
// 验证 InitChannelCache 读取 channel_model_disabled 并把被禁用的 (channel, model) 从候选池剔除
func TestInitChannelCache_FiltersDisabled(t *testing.T) {
	cleanupHealthTables(t)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Exec("DELETE FROM channels").Error)
		require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
		cleanupHealthTables(t)
	})

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })

	ch := &Channel{
		Id:     100,
		Type:   1,
		Name:   "test-channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
		Models: "model-a,model-b,model-c",
		Group:  "default",
	}
	require.NoError(t, DB.Create(ch).Error)

	for _, m := range []string{"model-a", "model-b", "model-c"} {
		require.NoError(t, DB.Create(&Ability{
			Group:     "default",
			Model:     m,
			ChannelId: 100,
			Enabled:   true,
		}).Error)
	}

	InitChannelCache()
	assert.Contains(t, group2model2channels["default"], "model-a")
	assert.Contains(t, group2model2channels["default"], "model-b")
	assert.Contains(t, group2model2channels["default"], "model-c")
	assert.Contains(t, group2model2channels["default"]["model-b"], 100)

	require.NoError(t, UpsertChannelModelDisabled(100, "model-b", DisabledSourceAuto, "test"))

	InitChannelCache()

	assert.Contains(t, group2model2channels["default"], "model-a")
	assert.Contains(t, group2model2channels["default"]["model-a"], 100)
	assert.Contains(t, group2model2channels["default"], "model-c")
	assert.Contains(t, group2model2channels["default"]["model-c"], 100)

	if chans, ok := group2model2channels["default"]["model-b"]; ok {
		assert.NotContains(t, chans, 100, "channel 100 的 model-b 已被禁用，不应出现在候选池")
	}

	require.NoError(t, DeleteAutoOrRelayDisabled(100, "model-b"))
	InitChannelCache()
	assert.Contains(t, group2model2channels["default"], "model-b")
	assert.Contains(t, group2model2channels["default"]["model-b"], 100)
}

// TestRemoveChannelModelFromCache
// 验证真实请求失败后可立即从内存候选池剔除指定 (channel, model)
func TestRemoveChannelModelFromCache(t *testing.T) {
	cleanupHealthTables(t)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Exec("DELETE FROM channels").Error)
		require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
		cleanupHealthTables(t)
	})

	prev := common.MemoryCacheEnabled
	common.MemoryCacheEnabled = true
	t.Cleanup(func() { common.MemoryCacheEnabled = prev })

	require.NoError(t, DB.Create(&Channel{
		Id:     101,
		Type:   1,
		Name:   "test-channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
		Models: "model-a,model-b",
		Group:  "default",
	}).Error)
	for _, m := range []string{"model-a", "model-b"} {
		require.NoError(t, DB.Create(&Ability{
			Group:     "default",
			Model:     m,
			ChannelId: 101,
			Enabled:   true,
		}).Error)
	}

	InitChannelCache()
	require.Contains(t, group2model2channels["default"]["model-b"], 101)

	RemoveChannelModelFromCache(101, "model-b")

	assert.Contains(t, group2model2channels["default"]["model-a"], 101)
	if chans, ok := group2model2channels["default"]["model-b"]; ok {
		assert.NotContains(t, chans, 101)
	}
}

// TestGetChannelExcludesDisabledWithoutMemoryCache
// 验证未启用内存缓存时，DB 直查路径也会过滤模型级禁用
func TestGetChannelExcludesDisabledWithoutMemoryCache(t *testing.T) {
	cleanupHealthTables(t)
	require.NoError(t, DB.Exec("DELETE FROM channels").Error)
	require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
	t.Cleanup(func() {
		require.NoError(t, DB.Exec("DELETE FROM channels").Error)
		require.NoError(t, DB.Exec("DELETE FROM abilities").Error)
		cleanupHealthTables(t)
	})

	for _, ch := range []*Channel{
		{
			Id:     111,
			Type:   1,
			Name:   "disabled-model-channel",
			Key:    "sk-test-1",
			Status: common.ChannelStatusEnabled,
			Models: "model-x",
			Group:  "default",
		},
		{
			Id:     112,
			Type:   1,
			Name:   "healthy-model-channel",
			Key:    "sk-test-2",
			Status: common.ChannelStatusEnabled,
			Models: "model-x",
			Group:  "default",
		},
	} {
		require.NoError(t, DB.Create(ch).Error)
		require.NoError(t, DB.Create(&Ability{
			Group:     "default",
			Model:     "model-x",
			ChannelId: ch.Id,
			Enabled:   true,
		}).Error)
	}
	require.NoError(t, UpsertChannelModelDisabled(111, "model-x", DisabledSourceRelay, "test"))

	ch, err := GetChannel("default", "model-x", 0, "")

	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, 112, ch.Id)
}
