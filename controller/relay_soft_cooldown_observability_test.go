package controller

import (
	"net/http"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessChannelErrorIncludesSoftCooldownAdminInfo(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.User{}))
	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalErrorLogEnabled := constant.ErrorLogEnabled
	originalRedisEnabled := common.RedisEnabled
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalHealthSetting := *healthSetting
	model.DB = db
	model.LOG_DB = db
	constant.ErrorLogEnabled = true
	common.RedisEnabled = false
	healthSetting.SoftFailureCooldownEnabled = true
	healthSetting.SoftFailureCooldownSeconds = 23
	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		constant.ErrorLogEnabled = originalErrorLogEnabled
		common.RedisEnabled = originalRedisEnabled
		*healthSetting = originalHealthSetting
	})

	ctx := newRetryTestContext()
	ctx.Set("id", 1)
	ctx.Set("channel_id", 9361)
	ctx.Set("channel_name", "soft-cooldown-error-log")
	ctx.Set("channel_type", 1)
	ctx.Set("original_model", "claude-error-log")
	service.RecordChannelSoftCooldown(ctx, 9361, "claude-error-log", 429, "rate_limit_error")
	relayErr := types.WithOpenAIError(types.OpenAIError{
		Message: "upstream rate limit exceeded",
		Type:    "rate_limit_error",
		Code:    "rate_limit_error",
	}, http.StatusTooManyRequests)

	processChannelError(ctx, *types.NewChannelError(9361, 1, "soft-cooldown-error-log", false, "", false), relayErr)

	var log model.Log
	require.NoError(t, db.Where("type = ?", model.LogTypeError).First(&log).Error)
	other, err := common.StrToMap(log.Other)
	require.NoError(t, err)
	adminInfo, ok := other["admin_info"].(map[string]interface{})
	require.True(t, ok)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, softCooldown["applied"])
	assert.Equal(t, float64(23), softCooldown["seconds"])
	assert.Equal(t, "rate_limit", softCooldown["reason_class"])
}
