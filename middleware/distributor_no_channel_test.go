package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func distributeChatForTest(t *testing.T, modelName string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	})
	router.Use(Distribute())
	router.POST("/v1/chat/completions", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+modelName+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// 一个渠道都没配：没授权或模型不存在，保留明确提示，不说成繁忙让用户白等。
func TestDistributeKeepsModelNotFoundWhenModelHasNoChannels(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	model.InitChannelCache()

	rec := distributeChatForTest(t, "model-never-configured")

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), string(types.ErrorCodeModelNotFound))
	assert.NotContains(t, rec.Body.String(), service.UpstreamUnavailableMessage)
	assert.Equal(t, "false", rec.Header().Get("X-Should-Retry"))
}

// 配过渠道但都被系统禁用：属于渠道故障，回统一繁忙提示，并告诉客户端不用再重试。
func TestDistributeReturnsBusyWhenConfiguredChannelsAreAllDisabled(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	setupDistributorSoftCooldown(t, true, 30)
	modelName := "model-all-disabled"
	seedDistributorChannel(t, 4301, modelName, 10)
	require.NoError(t, model.DB.Create(&model.ChannelModelDisabled{ChannelId: 4301, Model: modelName, Source: model.DisabledSourceRelay}).Error)
	model.InitChannelCache()

	rec := distributeChatForTest(t, modelName)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Contains(t, rec.Body.String(), service.UpstreamUnavailableMessage)
	assert.Equal(t, "false", rec.Header().Get("X-Should-Retry"))
}
