package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkImageFixedRouteRejectsUnauthorizedAndCoolsAcrossRequests(t *testing.T) {
	name := uniqueRelaySoftCooldownModelName("image-routing")
	channels := setupRelaySoftCooldownChannels(t, name, 2, 3, 5)
	require.NoError(t, model.DB.AutoMigrate(&model.Token{}, &model.TokenModelChannel{}))
	require.NoError(t, model.DB.Create(&model.Token{Id: 991, UserId: 1, Key: "local-image-route", Status: 1, ExpiredTime: -1, Group: "default", ModelLimitsEnabled: true, ModelLimits: name}).Error)
	for _, channel := range channels {
		require.NoError(t, model.DB.Model(channel).Updates(map[string]any{"type": constant.ChannelTypeOpenAI, "setting": `{"pass_through_body_enabled":true}`}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 991, ModelId: name, ChannelId: channel.Id}).Error)
	}
	model.InitChannelCache()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	old := client.Transport
	t.Cleanup(func() { client.Transport = old })
	attempts := 0
	client.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		attempts++
		assert.Empty(t, req.Header.Get("X-Twork-Image-Channel-Id"))
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","code":"overloaded","message":"busy"}}`)), Request: req}, nil
	})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyTokenId, 991)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	router.Use(middleware.Distribute())
	router.POST("/v1/images/generations", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIImage) })
	run := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"`+name+`","prompt":"cat","n":1}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Twork-Image-Channel-Id", strconv.Itoa(channels[0].Id))
		response := httptest.NewRecorder()
		router.ServeHTTP(response, req)
		return response
	}
	first := run()
	require.Equal(t, 503, first.Code, first.Body.String())
	assert.Equal(t, 1, attempts, "固定渠道不能在网关内部切换")
	second := run()
	require.Equal(t, 503, second.Code, second.Body.String())
	assert.Contains(t, second.Body.String(), "冷却")
	assert.Equal(t, 1, attempts, "冷却后的新请求不能到达上游")
	require.NoError(t, model.DB.Where("token_id = ?", 991).Delete(&model.TokenModelChannel{}).Error)
	denied := run()
	assert.Equal(t, 403, denied.Code, denied.Body.String())
	assert.Equal(t, 1, attempts, "root所属令牌也不能绕过已撤销授权")
}
