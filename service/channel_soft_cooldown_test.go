package service

import (
	"fmt"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupChannelSoftCooldownTest(t *testing.T, enabled bool, ttlSeconds int) {
	t.Helper()

	setting := operation_setting.GetChannelHealthSetting()
	originalSetting := *setting
	originalRedisEnabled := common.RedisEnabled
	setting.SoftFailureCooldownEnabled = enabled
	setting.SoftFailureCooldownSeconds = ttlSeconds
	common.RedisEnabled = false

	t.Cleanup(func() {
		*setting = originalSetting
		common.RedisEnabled = originalRedisEnabled
	})
}

func cleanupChannelSoftCooldownKey(t *testing.T, channelID int, modelName string) {
	t.Helper()
	t.Cleanup(func() {
		_, _ = getChannelSoftCooldownCache().DeleteMany([]string{fmt.Sprintf("%d:%s", channelID, modelName)})
	})
}

func TestRecordAndGetChannelSoftCooldown(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 1)
	const channelID = 91001
	const modelName = "claude-test-record"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)

	before := time.Now()
	RecordChannelSoftCooldown(channelID, modelName, 529, "overloaded")
	entry, cooling := GetChannelSoftCooldown(channelID, modelName)

	require.True(t, cooling)
	assert.Equal(t, 529, entry.StatusCode)
	assert.Equal(t, "overloaded", entry.ErrorClass)
	assert.True(t, entry.ExpiresAt.After(before))
	require.Eventually(t, func() bool {
		_, cooling := GetChannelSoftCooldown(channelID, modelName)
		return !cooling
	}, 2*time.Second, 20*time.Millisecond)
}

func TestChannelSoftCooldownKeysAreIsolated(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 1)
	const channelID = 91002
	const otherChannelID = 91003
	const modelName = "claude-test-isolation"
	const otherModelName = "claude-test-isolation-other"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)
	cleanupChannelSoftCooldownKey(t, channelID, otherModelName)
	cleanupChannelSoftCooldownKey(t, otherChannelID, modelName)

	RecordChannelSoftCooldown(channelID, modelName, 429, "rate_limited")

	_, cooling := GetChannelSoftCooldown(channelID, modelName)
	assert.True(t, cooling)
	_, cooling = GetChannelSoftCooldown(channelID, otherModelName)
	assert.False(t, cooling)
	_, cooling = GetChannelSoftCooldown(otherChannelID, modelName)
	assert.False(t, cooling)
}

func TestRecordChannelSoftCooldownResetsTTL(t *testing.T) {
	setupChannelSoftCooldownTest(t, true, 1)
	const channelID = 91004
	const modelName = "claude-test-reset-ttl"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)

	RecordChannelSoftCooldown(channelID, modelName, 529, "overloaded")
	first, cooling := GetChannelSoftCooldown(channelID, modelName)
	require.True(t, cooling)

	time.Sleep(300 * time.Millisecond)
	RecordChannelSoftCooldown(channelID, modelName, 429, "rate_limited")
	second, cooling := GetChannelSoftCooldown(channelID, modelName)
	require.True(t, cooling)
	assert.True(t, second.ExpiresAt.After(first.ExpiresAt))
	assert.Equal(t, 429, second.StatusCode)
	assert.Equal(t, "rate_limited", second.ErrorClass)

	time.Sleep(time.Until(first.ExpiresAt) + 50*time.Millisecond)
	_, cooling = GetChannelSoftCooldown(channelID, modelName)
	assert.True(t, cooling, "重复软故障应从最近一次写入重新计算 TTL")
}

func TestChannelSoftCooldownDisabled(t *testing.T) {
	setupChannelSoftCooldownTest(t, false, 1)
	const channelID = 91005
	const modelName = "claude-test-disabled"
	cleanupChannelSoftCooldownKey(t, channelID, modelName)

	RecordChannelSoftCooldown(channelID, modelName, 529, "overloaded")
	entry, cooling := GetChannelSoftCooldown(channelID, modelName)

	assert.False(t, cooling)
	assert.Zero(t, entry)
}
