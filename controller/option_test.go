package controller

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type optionUpdateTestResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

func setupOptionUpdateTest(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Option{}, &model.Log{}, &model.User{}))

	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalRedisEnabled := common.RedisEnabled
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalHealthSetting := *healthSetting
	keys := []string{
		"channel_health_setting.soft_failure_cooldown_enabled",
		"channel_health_setting.soft_failure_cooldown_seconds",
		"channel_health_setting.soft_failure_max_attempts",
	}
	type optionMapValue struct {
		value string
		set   bool
	}
	originalOptionValues := make(map[string]optionMapValue, len(keys))
	common.OptionMapRWMutex.Lock()
	optionMapWasNil := common.OptionMap == nil
	if optionMapWasNil {
		common.OptionMap = make(map[string]string)
	}
	for _, key := range keys {
		value, set := common.OptionMap[key]
		originalOptionValues[key] = optionMapValue{value: common.Interface2String(value), set: set}
	}
	common.OptionMapRWMutex.Unlock()

	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		common.RedisEnabled = originalRedisEnabled
		*healthSetting = originalHealthSetting
		common.OptionMapRWMutex.Lock()
		defer common.OptionMapRWMutex.Unlock()
		if optionMapWasNil {
			common.OptionMap = nil
			return
		}
		for key, original := range originalOptionValues {
			if original.set {
				common.OptionMap[key] = original.value
			} else {
				delete(common.OptionMap, key)
			}
		}
	})
}

func requestOptionUpdate(t *testing.T, key string, value any) optionUpdateTestResponse {
	t.Helper()
	payload, err := common.Marshal(OptionUpdateRequest{Key: key, Value: value})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPut, "/api/option/", strings.NewReader(string(payload)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	UpdateOption(ctx)

	assert.Equal(t, http.StatusOK, recorder.Code)
	var response optionUpdateTestResponse
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response
}

func TestUpdateOptionRejectsInvalidSoftFailureCooldownSettings(t *testing.T) {
	setupOptionUpdateTest(t)
	tests := []struct {
		name    string
		key     string
		value   any
		message string
	}{
		{
			name:    "cooldown seconds must be positive",
			key:     "channel_health_setting.soft_failure_cooldown_seconds",
			value:   -1,
			message: "软故障冷却时间必须是 1 到 86400 之间的整数",
		},
		{
			name:    "cooldown seconds rejects excessive values",
			key:     "channel_health_setting.soft_failure_cooldown_seconds",
			value:   86401,
			message: "软故障冷却时间必须是 1 到 86400 之间的整数",
		},
		{
			name:    "max attempts rejects values below range",
			key:     "channel_health_setting.soft_failure_max_attempts",
			value:   0,
			message: "软故障最大尝试次数必须是 1 到 10 之间的整数",
		},
		{
			name:    "max attempts rejects values above range",
			key:     "channel_health_setting.soft_failure_max_attempts",
			value:   11,
			message: "软故障最大尝试次数必须是 1 到 10 之间的整数",
		},
		{
			name:    "enabled rejects invalid boolean",
			key:     "channel_health_setting.soft_failure_cooldown_enabled",
			value:   "enabled",
			message: "软故障冷却开关必须是合法的布尔值",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := requestOptionUpdate(t, tt.key, tt.value)

			assert.False(t, response.Success)
			assert.Equal(t, tt.message, response.Message)
			var count int64
			require.NoError(t, model.DB.Model(&model.Option{}).Where("key = ?", tt.key).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestUpdateOptionAcceptsValidSoftFailureCooldownSettings(t *testing.T) {
	setupOptionUpdateTest(t)
	tests := []struct {
		name      string
		key       string
		value     any
		wantValue string
		verify    func(*testing.T)
	}{
		{
			name:      "enabled",
			key:       "channel_health_setting.soft_failure_cooldown_enabled",
			value:     true,
			wantValue: "true",
			verify: func(t *testing.T) {
				assert.True(t, operation_setting.IsSoftFailureCooldownEnabled())
			},
		},
		{
			name:      "cooldown seconds",
			key:       "channel_health_setting.soft_failure_cooldown_seconds",
			value:     60,
			wantValue: "60",
			verify: func(t *testing.T) {
				assert.Equal(t, 60, operation_setting.GetSoftFailureCooldownSeconds())
			},
		},
		{
			name:      "max attempts",
			key:       "channel_health_setting.soft_failure_max_attempts",
			value:     10,
			wantValue: "10",
			verify: func(t *testing.T) {
				assert.Equal(t, 10, operation_setting.GetSoftFailureMaxAttempts())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response := requestOptionUpdate(t, tt.key, tt.value)

			assert.True(t, response.Success)
			assert.Empty(t, response.Message)
			var option model.Option
			require.NoError(t, model.DB.Where("key = ?", tt.key).First(&option).Error)
			assert.Equal(t, tt.wantValue, option.Value)
			tt.verify(t)
		})
	}
}
