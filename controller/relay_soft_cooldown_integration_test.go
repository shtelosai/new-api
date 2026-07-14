package controller

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

var relaySoftCooldownTestSequence atomic.Int64

func uniqueRelaySoftCooldownModelName(base string) string {
	return fmt.Sprintf("%s-%d", base, relaySoftCooldownTestSequence.Add(1))
}

func setupRelaySoftCooldownChannels(t *testing.T, modelName string, channelCount int, retryTimes int, maxAttempts int) []*model.Channel {
	t.Helper()

	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ChannelModelDisabled{}))

	originalDB := model.DB
	originalLogDB := model.LOG_DB
	originalMainDatabaseType := common.MainDatabaseType()
	originalLogDatabaseType := common.LogDatabaseType()
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalRedisEnabled := common.RedisEnabled
	originalLogConsumeEnabled := common.LogConsumeEnabled
	originalErrorLogEnabled := constant.ErrorLogEnabled
	originalRetryTimes := common.RetryTimes
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalHealthSetting := *healthSetting
	quotaSetting := operation_setting.GetQuotaSetting()
	originalQuotaSetting := *quotaSetting
	originalModelRatios, err := common.Marshal(ratio_setting.GetModelRatioCopy())
	require.NoError(t, err)
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(fmt.Sprintf(`{"%s":0}`, modelName)))

	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = true
	common.RedisEnabled = false
	common.LogConsumeEnabled = false
	constant.ErrorLogEnabled = false
	common.RetryTimes = retryTimes
	healthSetting.SoftFailureCooldownEnabled = true
	healthSetting.SoftFailureCooldownSeconds = 30
	healthSetting.SoftFailureMaxAttempts = maxAttempts
	quotaSetting.EnableFreeModelPreConsume = false

	t.Cleanup(func() {
		model.DB = originalDB
		model.LOG_DB = originalLogDB
		common.SetDatabaseTypes(originalMainDatabaseType, originalLogDatabaseType)
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.RedisEnabled = originalRedisEnabled
		common.LogConsumeEnabled = originalLogConsumeEnabled
		constant.ErrorLogEnabled = originalErrorLogEnabled
		common.RetryTimes = originalRetryTimes
		*healthSetting = originalHealthSetting
		*quotaSetting = originalQuotaSetting
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(string(originalModelRatios)))
		if originalDB != nil {
			model.InitChannelCache()
		}
	})

	channels := make([]*model.Channel, 0, channelCount)
	for i := 0; i < channelCount; i++ {
		channelID := 9700 + i
		priority := int64(channelCount - i)
		weight := uint(100)
		autoBan := 0
		ratio := float64(0)
		baseURL := fmt.Sprintf("https://channel-%d.test", channelID)
		channel := &model.Channel{
			Id:       channelID,
			Type:     constant.ChannelTypeAnthropic,
			Name:     fmt.Sprintf("channel-%d", channelID),
			Key:      "sk-test",
			Status:   common.ChannelStatusEnabled,
			Models:   modelName,
			Group:    "default",
			Priority: &priority,
			Weight:   &weight,
			AutoBan:  &autoBan,
			Ratio:    &ratio,
			BaseURL:  &baseURL,
		}
		require.NoError(t, db.Create(channel).Error)
		require.NoError(t, db.Create(&model.Ability{
			Group:     "default",
			Model:     modelName,
			ChannelId: channelID,
			Enabled:   true,
			Priority:  &priority,
			Weight:    weight,
		}).Error)
		channels = append(channels, channel)
	}
	model.InitChannelCache()
	return channels
}

func runClaudeRelayWithStatuses(t *testing.T, modelName string, firstChannel *model.Channel, statuses []int) (*httptest.ResponseRecorder, int) {
	t.Helper()
	require.NotEmpty(t, statuses)
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	httpClient := service.GetHttpClient()
	require.NotNil(t, httpClient)
	originalTransport := httpClient.Transport
	attempts := 0
	httpClient.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		status := statuses[min(attempts, len(statuses)-1)]
		attempts++
		body := `{"type":"error","error":{"type":"api_error","message":"upstream internal failure"}}`
		if status == 529 {
			body = `{"type":"error","error":{"type":"overloaded_error","message":"upstream service overloaded"}}`
		} else if status == http.StatusOK {
			body = `{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { httpClient.Transport = originalTransport })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(
		`{"model":"%s","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`,
		modelName,
	)))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, firstChannel, modelName))

	Relay(c, types.RelayFormatClaude)
	return w, attempts
}

func configureClaudeAffinityForRelayTest(t *testing.T) {
	t.Helper()
	setting := operation_setting.GetChannelAffinitySetting()
	originalSetting := *setting
	setting.Enabled = true
	setting.SwitchOnSuccess = true
	setting.KeepOnChannelDisabled = false
	setting.DefaultTTLSeconds = 3600
	setting.Rules = []operation_setting.ChannelAffinityRule{
		{
			Name:              "claude relay integration",
			ModelRegex:        []string{"^claude-.*$"},
			PathRegex:         []string{"^/v1/messages$"},
			KeySources:        []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "metadata.user_id"}},
			IncludeUsingGroup: true,
			IncludeRuleName:   true,
		},
	}
	t.Cleanup(func() { *setting = originalSetting })
}

func seedClaudeAffinityForRelayTest(t *testing.T, modelName string, affinityKey string, channelID int) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	_, found := service.GetPreferredChannelByAffinity(c, modelName, "default")
	require.False(t, found)
	service.RecordChannelAffinity(c, channelID)
	t.Cleanup(func() { service.ClearCurrentChannelAffinityCache(c) })
}

func requireClaudeAffinityChannelForRelayTest(t *testing.T, modelName string, affinityKey string, channelID int) {
	t.Helper()
	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")

	actualChannelID, found := service.GetPreferredChannelByAffinity(c, modelName, "default")
	require.True(t, found)
	require.Equal(t, channelID, actualChannelID)
}

func runClaudeRelayThroughDistribute(
	t *testing.T,
	modelName string,
	affinityKey string,
	statusForHost func(string) int,
) (*httptest.ResponseRecorder, []string, map[string]interface{}) {
	t.Helper()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	httpClient := service.GetHttpClient()
	require.NotNil(t, httpClient)
	originalTransport := httpClient.Transport
	requestedHosts := make([]string, 0, 2)
	httpClient.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		requestedHosts = append(requestedHosts, req.URL.Host)
		status := statusForHost(req.URL.Host)
		body := `{"type":"error","error":{"type":"overloaded_error","message":"upstream service overloaded"}}`
		if status == http.StatusOK {
			body = `{"id":"msg_integration","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	t.Cleanup(func() { httpClient.Transport = originalTransport })

	adminInfo := map[string]interface{}{}
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	router.Use(middleware.Distribute())
	router.POST("/v1/messages", func(c *gin.Context) {
		Relay(c, types.RelayFormatClaude)
		service.AppendChannelSoftCooldownAdminInfo(c, adminInfo)
	})
	body := fmt.Sprintf(
		`{"model":"%s","max_tokens":16,"metadata":{"user_id":"%s"},"messages":[{"role":"user","content":"hello"}]}`,
		modelName,
		affinityKey,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w, requestedHosts, adminInfo
}

func TestRelaySoftFailureRetriesCanUseExpandedAttemptBudget(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("claude-soft-retry-budget")
	channels := setupRelaySoftCooldownChannels(t, modelName, 5, 3, 5)

	w, attempts := runClaudeRelayWithStatuses(t, modelName, channels[0], []int{529, 529, 529, 529, http.StatusOK})

	require.Equal(t, 5, attempts)
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"id":"msg_test"`)
}

func TestRelayNonSoftFailureKeepsGlobalRetryBudget(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("claude-hard-retry-budget")
	channels := setupRelaySoftCooldownChannels(t, modelName, 5, 3, 5)

	w, attempts := runClaudeRelayWithStatuses(t, modelName, channels[0], []int{
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
		http.StatusInternalServerError,
	})

	require.Equal(t, 4, attempts)
	require.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestRelayMixedFailuresUseBudgetForLatestFailure(t *testing.T) {
	t.Run("soft failure followed by non-soft failure stops at global budget", func(t *testing.T) {
		modelName := uniqueRelaySoftCooldownModelName("claude-mixed-budget-soft-hard")
		channels := setupRelaySoftCooldownChannels(t, modelName, 4, 1, 4)

		w, attempts := runClaudeRelayWithStatuses(t, modelName, channels[0], []int{529, http.StatusInternalServerError, http.StatusOK})

		require.Equal(t, 2, attempts)
		require.Equal(t, http.StatusInternalServerError, w.Code)
	})

	t.Run("non-soft failure followed by soft failures can use expanded budget", func(t *testing.T) {
		modelName := uniqueRelaySoftCooldownModelName("claude-mixed-budget-hard-soft")
		channels := setupRelaySoftCooldownChannels(t, modelName, 4, 1, 4)

		w, attempts := runClaudeRelayWithStatuses(t, modelName, channels[0], []int{http.StatusInternalServerError, 529, 529, http.StatusOK})

		require.Equal(t, 4, attempts)
		require.Equal(t, http.StatusOK, w.Code)
	})
}

func TestRelayCountsSuccessAfterCoolingAffinityIsCleared(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("claude-affinity-cleared-outcome")
	affinityKey := "affinity-cleared-outcome"
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channels[0].Id).Update("priority", 1).Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", channels[0].Id).Update("priority", 1).Error)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channels[1].Id).Update("priority", 2).Error)
	require.NoError(t, model.DB.Model(&model.Ability{}).Where("channel_id = ?", channels[1].Id).Update("priority", 2).Error)
	model.InitChannelCache()
	configureClaudeAffinityForRelayTest(t)
	seedClaudeAffinityForRelayTest(t, modelName, affinityKey, channels[0].Id)
	service.RecordChannelSoftCooldown(nil, channels[0].Id, modelName, 529, "overloaded_error")
	outcomeBefore := gatheredCounterValue(t, "newapi_channel_soft_failover_total", map[string]string{
		"outcome": service.SoftFailoverOutcomeSameRequestSuccess,
	})

	w, requestedHosts, adminInfo := runClaudeRelayThroughDistribute(t, modelName, affinityKey, func(host string) int {
		if host == "channel-9701.test" {
			return http.StatusOK
		}
		return http.StatusInternalServerError
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, []string{"channel-9701.test"}, requestedHosts)
	requireClaudeAffinityChannelForRelayTest(t, modelName, affinityKey, channels[1].Id)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, true, softCooldown["affinity_cleared"])
	require.Equal(t, outcomeBefore+1, gatheredCounterValue(t, "newapi_channel_soft_failover_total", map[string]string{
		"outcome": service.SoftFailoverOutcomeSameRequestSuccess,
	}))
}

func TestDistributeRelaySoftFailureFailoverRebindsAffinity(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("claude-full-soft-failover")
	affinityKey := "full-soft-failover"
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	configureClaudeAffinityForRelayTest(t)
	seedClaudeAffinityForRelayTest(t, modelName, affinityKey, channels[0].Id)
	appliedBefore := gatheredCounterValue(t, "newapi_channel_soft_cooldown_applied_total", map[string]string{
		"channel_id":  "9700",
		"error_class": "overloaded",
	})
	outcomeBefore := gatheredCounterValue(t, "newapi_channel_soft_failover_total", map[string]string{
		"outcome": service.SoftFailoverOutcomeSameRequestSuccess,
	})

	w, requestedHosts, adminInfo := runClaudeRelayThroughDistribute(t, modelName, affinityKey, func(host string) int {
		if host == "channel-9700.test" {
			return 529
		}
		return http.StatusOK
	})

	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), `"id":"msg_integration"`)
	require.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, requestedHosts)
	requireClaudeAffinityChannelForRelayTest(t, modelName, affinityKey, channels[1].Id)
	entry, cooling := service.GetChannelSoftCooldown(nil, channels[0].Id, modelName)
	require.True(t, cooling)
	require.Equal(t, "overloaded", entry.ErrorClass)
	softCooldown, ok := adminInfo["soft_cooldown"].(map[string]interface{})
	require.True(t, ok)
	require.Equal(t, true, softCooldown["applied"])
	require.Equal(t, "overloaded", softCooldown["reason_class"])
	require.Equal(t, false, softCooldown["cache_degraded"])
	require.Equal(t, false, softCooldown["affinity_cleared"])
	require.Equal(t, appliedBefore+1, gatheredCounterValue(t, "newapi_channel_soft_cooldown_applied_total", map[string]string{
		"channel_id":  "9700",
		"error_class": "overloaded",
	}))
	require.Equal(t, outcomeBefore+1, gatheredCounterValue(t, "newapi_channel_soft_failover_total", map[string]string{
		"outcome": service.SoftFailoverOutcomeSameRequestSuccess,
	}))
}
