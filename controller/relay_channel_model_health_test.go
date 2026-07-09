package controller

import (
	"net/http"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func withRelayModelHealthTestDB(t *testing.T) {
	t.Helper()
	origDB := model.DB
	origLOGDB := model.LOG_DB
	origMainDBType := common.MainDatabaseType()
	origLogDBType := common.LogDatabaseType()

	db, err := gorm.Open(sqlite.Open("file::memory:?cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelDisabled{}, &model.ChannelModelHealth{}))
	require.NoError(t, db.Exec("DELETE FROM channel_model_disabled").Error)
	require.NoError(t, db.Exec("DELETE FROM channel_model_health").Error)

	t.Cleanup(func() {
		model.DB = origDB
		model.LOG_DB = origLOGDB
		common.SetDatabaseTypes(origMainDBType, origLogDBType)
	})
}

func withRelayModelHealthAutoDisable(t *testing.T) {
	t.Helper()
	origGlobal := common.AutomaticDisableChannelEnabled
	origRanges := operation_setting.AutomaticDisableStatusCodeRanges
	healthSetting := operation_setting.GetChannelHealthSetting()
	origModelDisable := healthSetting.ModelLevelAutoDisableEnabled

	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 401, End: 401}}
	healthSetting.ModelLevelAutoDisableEnabled = true

	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = origGlobal
		operation_setting.AutomaticDisableStatusCodeRanges = origRanges
		healthSetting.ModelLevelAutoDisableEnabled = origModelDisable
	})
}

func TestProcessChannelErrorDisablesOriginalModelFromRelay(t *testing.T) {
	withRelayModelHealthTestDB(t)
	withRelayModelHealthAutoDisable(t)

	c := newRetryTestContext()
	c.Set("original_model", "f3-relay-model")
	err := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid api key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized)

	processChannelError(c, *types.NewChannelError(9101, 1, "relay-health-channel", false, "", true), err)

	require.Eventually(t, func() bool {
		disabled, lookupErr := model.IsChannelModelDisabled(9101, "f3-relay-model")
		return lookupErr == nil && disabled
	}, time.Second, 10*time.Millisecond)

	var row model.ChannelModelDisabled
	require.NoError(t, model.DB.Where("channel_id = ? AND model = ?", 9101, "f3-relay-model").First(&row).Error)
	assert.Equal(t, model.DisabledSourceRelay, row.Source)
	assert.Contains(t, row.Reason, "status_code=401")

	health, healthErr := model.GetChannelModelHealth(9101, "f3-relay-model")
	require.NoError(t, healthErr)
	assert.Equal(t, operation_setting.GetChannelHealthFailureThreshold(), health.ConsecutiveFailures)
}

func TestProcessChannelErrorSkipsModelDisableWhenOriginalModelEmpty(t *testing.T) {
	withRelayModelHealthTestDB(t)
	withRelayModelHealthAutoDisable(t)

	c := newRetryTestContext()
	err := types.WithOpenAIError(types.OpenAIError{
		Message: "invalid api key",
		Type:    "authentication_error",
		Code:    "invalid_api_key",
	}, http.StatusUnauthorized)

	processChannelError(c, *types.NewChannelError(9102, 1, "relay-health-channel", false, "", true), err)

	var count int64
	require.NoError(t, model.DB.Model(&model.ChannelModelDisabled{}).Count(&count).Error)
	assert.Equal(t, int64(0), count)
}
