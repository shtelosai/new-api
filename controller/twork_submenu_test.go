package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
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

func TestSubmenuRelayFailureNeverFallsBackToSameModelChannel(t *testing.T) {
	const name = "submenu-relay-model"
	channels := setupRelaySoftCooldownChannels(t, name, 2, 2, 3)
	require.NoError(t, model.DB.AutoMigrate(&model.TokenModelChannel{}))
	for _, ch := range channels {
		require.NoError(t, model.DB.Model(ch).Updates(map[string]any{"type": constant.ChannelTypeOpenAI, "setting": `{"twork_runtime":"pi","twork_wire_api":"chat_completions","twork_display_mode":"submenu"}`}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 8702, ModelId: name, ChannelId: ch.Id}).Error)
	}
	model.InitChannelCache()
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previous := client.Transport
	t.Cleanup(func() { client.Transport = previous })
	var hosts []string
	client.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"selected upstream failed","type":"server_error"}}`)), Request: req}, nil
	})
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, 8702)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	router.Use(middleware.Distribute())
	router.POST("/v1/chat/completions", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"submenu-relay-model","messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Twork-Client-Version", "4.0.0")
	req.Header.Set("X-Twork-Client-Capabilities", "model-routes-v2")
	req.Header.Set("X-Twork-Channel-Id", "9700")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	assert.Equal(t, http.StatusServiceUnavailable, recorder.Code, recorder.Body.String())
	assert.Len(t, hosts, 1)
	assert.Contains(t, recorder.Body.String(), "selected upstream failed")
}

func TestUpdateChannelPreservesCMSSubmenuWhenOldFormChangesProxy(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelHealth{}, &model.Log{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "submenu-admin", Role: common.RoleRootUser}).Error)
	stored := `{"twork_runtime":"pi","twork_wire_api":"responses","twork_pi_compatibility":"openai","twork_display_mode":"submenu"}`
	ch := model.Channel{Id: 4901, Type: constant.ChannelTypeOpenAI, Name: "submenu-channel", Key: "test-key", Models: "gpt-5", Group: "default", Setting: &stored}
	require.NoError(t, db.Create(&ch).Error)
	// 旧表单里的 legacy 与 submenu 冲突；必须先恢复 CMS 当前值再校验。
	body, err := common.Marshal(map[string]any{"id": 4901, "type": constant.ChannelTypeOpenAI, "name": "submenu-channel", "models": "gpt-5", "group": "default", "setting": `{"twork_runtime":"legacy","twork_display_mode":"submenu","proxy":"http://127.0.0.1:3001"}`})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("role", common.RoleRootUser)
	c.Set("id", 1)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/channel/", strings.NewReader(string(body)))
	c.Request.Header.Set("Content-Type", "application/json")
	UpdateChannel(c)
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success, recorder.Body.String())
	var persisted model.Channel
	require.NoError(t, db.First(&persisted, 4901).Error)
	var setting map[string]any
	require.NoError(t, common.UnmarshalJsonStr(*persisted.Setting, &setting))
	assert.Equal(t, map[string]any{"twork_runtime": "pi", "twork_wire_api": "responses", "twork_pi_compatibility": "openai", "twork_display_mode": "submenu", "proxy": "http://127.0.0.1:3001"}, setting)
}
