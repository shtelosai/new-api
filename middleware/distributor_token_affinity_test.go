package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupDistributorTokenAffinityDB(t *testing.T) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	prevDB := model.DB
	prevLOGDB := model.LOG_DB
	prevMemory := common.MemoryCacheEnabled
	prevRedis := common.RedisEnabled
	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.MemoryCacheEnabled = true
	common.RedisEnabled = false
	t.Cleanup(func() {
		model.DB = prevDB
		model.LOG_DB = prevLOGDB
		common.MemoryCacheEnabled = prevMemory
		common.RedisEnabled = prevRedis
	})

	require.NoError(t, db.AutoMigrate(
		&model.Channel{},
		&model.Ability{},
		&model.TokenModelChannel{},
		&model.ChannelModelDisabled{},
		&model.ChannelModelHealth{},
	))
}

func seedDistributorChannel(t *testing.T, id int, modelName string, priority int64) {
	t.Helper()
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:       id,
		Type:     1,
		Name:     fmt.Sprintf("channel-%d", id),
		Key:      "sk-test",
		Status:   common.ChannelStatusEnabled,
		Models:   modelName,
		Group:    "default",
		Priority: &priority,
	}).Error)
	require.NoError(t, model.DB.Create(&model.Ability{
		Group:     "default",
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
	}).Error)
}

func seedAffinityChannelForRequest(t *testing.T, requestPath string, requestBody string, modelName string, usingGroup string, channelID int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")
	_, found := service.GetPreferredChannelByAffinity(ctx, modelName, usingGroup)
	require.False(t, found)
	service.RecordChannelAffinity(ctx, channelID)
	t.Cleanup(func() {
		service.ClearCurrentChannelAffinityCache(ctx)
	})
}

func seedAffinityChannel(t *testing.T, modelName string, usingGroup string, affinityKey string, channelID int) {
	t.Helper()
	seedAffinityChannelForRequest(
		t,
		"/v1/responses",
		fmt.Sprintf(`{"model":"%s","prompt_cache_key":"%s"}`, modelName, affinityKey),
		modelName,
		usingGroup,
		channelID,
	)
}

type distributedChannelPayload struct {
	ChannelID    int                    `json:"channel_id"`
	SkipRetry    bool                   `json:"skip_retry"`
	SoftCooldown map[string]interface{} `json:"soft_cooldown"`
}

func requestDistributedChannelForRequest(t *testing.T, tokenID int, usingGroup string, requestPath string, requestBody string, status int, setup func(*gin.Context)) distributedChannelPayload {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, usingGroup)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, tokenID)
		if setup != nil {
			setup(c)
		}
	})
	router.Use(Distribute())
	router.POST(requestPath, func(c *gin.Context) {
		adminInfo := map[string]interface{}{}
		service.AppendChannelSoftCooldownAdminInfo(c, adminInfo)
		c.JSON(status, gin.H{
			"channel_id":    c.GetInt("channel_id"),
			"skip_retry":    service.ShouldSkipRetryAfterChannelAffinityFailure(c),
			"soft_cooldown": adminInfo["soft_cooldown"],
		})
	})

	req := httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, status, rec.Code)
	var payload distributedChannelPayload
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &payload))
	return payload
}

func requestDistributedChannel(t *testing.T, tokenID int, usingGroup string, affinityKey string, status int) int {
	t.Helper()
	payload := requestDistributedChannelForRequest(
		t,
		tokenID,
		usingGroup,
		"/v1/responses",
		fmt.Sprintf(`{"model":"gpt-5","prompt_cache_key":"%s"}`, affinityKey),
		status,
		nil,
	)
	return payload.ChannelID
}

func assertAffinityStillPointsTo(t *testing.T, modelName string, usingGroup string, affinityKey string, channelID int) {
	t.Helper()
	assertAffinityStillPointsToForRequest(
		t,
		"/v1/responses",
		fmt.Sprintf(`{"model":"%s","prompt_cache_key":"%s"}`, modelName, affinityKey),
		modelName,
		usingGroup,
		channelID,
	)
}

func assertAffinityStillPointsToForRequest(t *testing.T, requestPath string, requestBody string, modelName string, usingGroup string, channelID int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, requestPath, strings.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	got, found := service.GetPreferredChannelByAffinity(ctx, modelName, usingGroup)
	require.True(t, found)
	assert.Equal(t, channelID, got)
}

func TestDistributeAffinityHonorsTokenWhitelist(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	modelName := "gpt-5"
	affinityKey := fmt.Sprintf("shared-affinity-%d", time.Now().UnixNano())
	highPriority := int64(10)
	lowPriority := int64(0)
	seedDistributorChannel(t, 4101, modelName, highPriority)
	seedDistributorChannel(t, 4102, modelName, lowPriority)
	require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 901, ModelId: modelName, ChannelId: 4101}).Error)
	require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 902, ModelId: modelName, ChannelId: 4102}).Error)

	model.InitChannelCache()
	model.InitTokenModelChannelCache()
	require.Eventually(t, func() bool {
		return model.IsChannelAllowedForToken(901, modelName, 4101) &&
			!model.IsChannelAllowedForToken(901, modelName, 4102) &&
			model.IsChannelAllowedForToken(902, modelName, 4102) &&
			!model.IsChannelAllowedForToken(902, modelName, 4101)
	}, time.Second, 10*time.Millisecond)

	seedAffinityChannel(t, modelName, "default", affinityKey, 4101)
	seedAffinityChannel(t, modelName, "auto", affinityKey, 4101)

	assert.Equal(t, 4101, requestDistributedChannel(t, 901, "default", affinityKey, http.StatusUnavailableForLegalReasons))
	assert.Equal(t, 4102, requestDistributedChannel(t, 902, "default", affinityKey, http.StatusUnavailableForLegalReasons))
	assertAffinityStillPointsTo(t, modelName, "default", affinityKey, 4101)

	assert.Equal(t, 4101, requestDistributedChannel(t, 903, "default", affinityKey, http.StatusUnavailableForLegalReasons))
	assert.Equal(t, 4102, requestDistributedChannel(t, 902, "auto", affinityKey, http.StatusUnavailableForLegalReasons))
	assertAffinityStillPointsTo(t, modelName, "auto", affinityKey, 4101)
}

func setupDistributorSoftCooldown(t *testing.T, enabled bool, seconds int) {
	t.Helper()
	setting := operation_setting.GetChannelHealthSetting()
	originalSetting := *setting
	originalRedisEnabled := common.RedisEnabled
	setting.SoftFailureCooldownEnabled = enabled
	setting.SoftFailureCooldownSeconds = seconds
	common.RedisEnabled = false
	t.Cleanup(func() {
		*setting = originalSetting
		common.RedisEnabled = originalRedisEnabled
	})
}

func assertClaudeAffinityMissing(t *testing.T, modelName string, usingGroup string, affinityKey string) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	_, found := service.GetPreferredChannelByAffinity(ctx, modelName, usingGroup)
	assert.False(t, found)
}

func TestDistributeClaudeAffinityCoolingDeletesBindingAndFallsBack(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	affinitySetting := operation_setting.GetChannelAffinitySetting()
	originalRules := affinitySetting.Rules
	affinitySetting.Rules = append([]operation_setting.ChannelAffinityRule(nil), originalRules...)
	for i := range affinitySetting.Rules {
		if affinitySetting.Rules[i].Name == "claude cli trace" {
			affinitySetting.Rules[i].SkipRetryOnFailure = true
		}
	}
	t.Cleanup(func() { affinitySetting.Rules = originalRules })
	modelName := "claude-affinity-cooldown"
	affinityKey := fmt.Sprintf("claude-affinity-%d", time.Now().UnixNano())
	highPriority := int64(10)
	lowPriority := int64(0)
	seedDistributorChannel(t, 4201, modelName, highPriority)
	seedDistributorChannel(t, 4202, modelName, lowPriority)
	model.InitChannelCache()

	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)
	seedAffinityChannelForRequest(t, "/v1/messages", body, modelName, "default", 4201)
	service.RecordChannelSoftCooldown(nil, 4201, modelName, 529, "overloaded_error")

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/messages", body, http.StatusUnavailableForLegalReasons, nil)

	assert.Equal(t, 4202, payload.ChannelID)
	assert.True(t, payload.SkipRetry, "删除冷却 affinity 绑定不能改写本次请求原有的 skip-retry 策略")
	assert.Equal(t, true, payload.SoftCooldown["affinity_cleared"])
	assertClaudeAffinityMissing(t, modelName, "default", affinityKey)
}

func TestDistributeSoftCooldownDisabledKeepsClaudeAffinity(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "claude-affinity-cooldown-disabled"
	affinityKey := fmt.Sprintf("claude-affinity-disabled-%d", time.Now().UnixNano())
	highPriority := int64(10)
	lowPriority := int64(0)
	seedDistributorChannel(t, 4211, modelName, highPriority)
	seedDistributorChannel(t, 4212, modelName, lowPriority)
	model.InitChannelCache()

	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)
	seedAffinityChannelForRequest(t, "/v1/messages", body, modelName, "default", 4211)
	service.RecordChannelSoftCooldown(nil, 4211, modelName, 529, "overloaded_error")
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = false

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/messages", body, http.StatusUnavailableForLegalReasons, nil)

	assert.Equal(t, 4211, payload.ChannelID)
	assert.Nil(t, payload.SoftCooldown)
	assertAffinityStillPointsToForRequest(t, "/v1/messages", body, modelName, "default", 4211)
}

func TestDistributeNonClaudeRequestIgnoresSoftCooldown(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "gpt-5"
	affinityKey := fmt.Sprintf("responses-cooldown-%d", time.Now().UnixNano())
	highPriority := int64(10)
	lowPriority := int64(0)
	seedDistributorChannel(t, 4221, modelName, highPriority)
	seedDistributorChannel(t, 4222, modelName, lowPriority)
	model.InitChannelCache()
	seedAffinityChannel(t, modelName, "default", affinityKey, 4221)
	service.RecordChannelSoftCooldown(nil, 4221, modelName, 529, "overloaded_error")

	selected := requestDistributedChannel(t, 0, "default", affinityKey, http.StatusUnavailableForLegalReasons)

	assert.Equal(t, 4221, selected)
	assertAffinityStillPointsTo(t, modelName, "default", affinityKey, 4221)
}

func TestDistributeClaudeSoftCooldownDoesNotRecordAffinityWithoutRelaySuccess(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "claude-affinity-stream-failure"
	affinityKey := fmt.Sprintf("claude-stream-failure-%d", time.Now().UnixNano())
	priority := int64(10)
	seedDistributorChannel(t, 4251, modelName, priority)
	model.InitChannelCache()
	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/messages", body, http.StatusOK, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, true)
	})

	assert.Equal(t, 4251, payload.ChannelID)
	assertClaudeAffinityMissing(t, modelName, "default", affinityKey)
}

func TestDistributeClaudeSoftCooldownRecordsAffinityAfterRelaySuccess(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "claude-affinity-success"
	affinityKey := fmt.Sprintf("claude-success-%d", time.Now().UnixNano())
	priority := int64(10)
	seedDistributorChannel(t, 4252, modelName, priority)
	model.InitChannelCache()
	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/messages", body, http.StatusOK, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyClaudeRelaySucceeded, true)
	})

	assert.Equal(t, 4252, payload.ChannelID)
	assertAffinityStillPointsToForRequest(t, "/v1/messages", body, modelName, "default", 4252)
}

func TestDistributeClaudeSoftCooldownDisabledKeepsStatusBasedAffinityRecording(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, false, 30)
	modelName := "claude-affinity-disabled-stream-failure"
	affinityKey := fmt.Sprintf("claude-disabled-stream-failure-%d", time.Now().UnixNano())
	priority := int64(10)
	seedDistributorChannel(t, 4253, modelName, priority)
	model.InitChannelCache()
	body := fmt.Sprintf(`{"model":"%s","metadata":{"user_id":"%s"}}`, modelName, affinityKey)

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/messages", body, http.StatusOK, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyClaudeStreamCommitted, true)
	})

	assert.Equal(t, 4253, payload.ChannelID)
	assertAffinityStillPointsToForRequest(t, "/v1/messages", body, modelName, "default", 4253)
}

func TestDistributeNonClaudeKeepsStatusBasedAffinityRecording(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "gpt-5"
	affinityKey := fmt.Sprintf("responses-success-%d", time.Now().UnixNano())
	priority := int64(10)
	seedDistributorChannel(t, 4254, modelName, priority)
	model.InitChannelCache()
	body := fmt.Sprintf(`{"model":"%s","prompt_cache_key":"%s"}`, modelName, affinityKey)

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/responses", body, http.StatusOK, nil)

	assert.Equal(t, 4254, payload.ChannelID)
	assertAffinityStillPointsToForRequest(t, "/v1/responses", body, modelName, "default", 4254)
}

func TestDistributeSpecificChannelBypassesSoftCooldown(t *testing.T) {
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "claude-specific-cooldown"
	priority := int64(10)
	seedDistributorChannel(t, 4231, modelName, priority)
	model.InitChannelCache()
	service.RecordChannelSoftCooldown(nil, 4231, modelName, 529, "overloaded_error")
	body := fmt.Sprintf(`{"model":"%s"}`, modelName)

	payload := requestDistributedChannelForRequest(t, 0, "default", "/v1/messages", body, http.StatusUnavailableForLegalReasons, func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "4231")
	})

	assert.Equal(t, 4231, payload.ChannelID)
}

func TestDistributeAllClaudeChannelsCoolingReturnsRetryAfter(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "claude-all-cooling"
	highPriority := int64(10)
	lowPriority := int64(0)
	seedDistributorChannel(t, 4241, modelName, highPriority)
	seedDistributorChannel(t, 4242, modelName, lowPriority)
	model.InitChannelCache()
	service.RecordChannelSoftCooldown(nil, 4241, modelName, 529, "overloaded_error")
	service.RecordChannelSoftCooldown(nil, 4242, modelName, 529, "overloaded_error")

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	})
	router.Use(Distribute())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(fmt.Sprintf(`{"model":"%s"}`, modelName)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, "30", rec.Header().Get("Retry-After"))
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &payload))
	assert.Equal(t, string(types.ErrorCodeModelNotFound), payload.Error.Code)
}

func TestDistributeClaudeWithoutCandidatesDoesNotSetRetryAfter(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)

	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	})
	router.Use(Distribute())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Status(http.StatusNoContent)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-no-candidates"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	router.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Empty(t, rec.Header().Get("Retry-After"))
}
