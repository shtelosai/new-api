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
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
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

func seedAffinityChannel(t *testing.T, modelName string, usingGroup string, affinityKey string, channelID int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"%s","prompt_cache_key":"%s"}`, modelName, affinityKey)))
	ctx.Request.Header.Set("Content-Type", "application/json")
	_, found := service.GetPreferredChannelByAffinity(ctx, modelName, usingGroup)
	require.False(t, found)
	service.RecordChannelAffinity(ctx, channelID)
	t.Cleanup(func() {
		service.ClearCurrentChannelAffinityCache(ctx)
	})
}

func requestDistributedChannel(t *testing.T, tokenID int, usingGroup string, affinityKey string, status int) int {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, usingGroup)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, tokenID)
	})
	router.Use(Distribute())
	router.POST("/v1/responses", func(c *gin.Context) {
		c.JSON(status, gin.H{"channel_id": c.GetInt("channel_id")})
	})

	body := strings.NewReader(fmt.Sprintf(`{"model":"gpt-5","prompt_cache_key":"%s"}`, affinityKey))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, status, rec.Code)
	var payload struct {
		ChannelID int `json:"channel_id"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &payload))
	return payload.ChannelID
}

func assertAffinityStillPointsTo(t *testing.T, modelName string, usingGroup string, affinityKey string, channelID int) {
	t.Helper()
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"%s","prompt_cache_key":"%s"}`, modelName, affinityKey)))
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
