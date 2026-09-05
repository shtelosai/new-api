package middleware

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDistributeExplicitRuntimeRoute(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, tc := range []struct {
		name, id, version, capability, path, setting                                   string
		grant, disabled, health, limited, dbFailure, specific, memoryOff, limitAllowed bool
		want                                                                           int
	}{
		{name: "authorized cold cache", grant: true, want: 200},
		{name: "authorized without memory cache", grant: true, memoryOff: true, want: 200},
		{name: "compact token model limit", grant: true, path: "/v1/responses/compact", limitAllowed: true, want: 200},
		{name: "compact", grant: true, path: "/v1/responses/compact", want: 200},
		{name: "build metadata", grant: true, version: "4.0.0+build.1", want: 200},
		{name: "no explicit grant including root", want: 403},
		{name: "disabled", grant: true, disabled: true, want: 403},
		{name: "model disabled", grant: true, health: true, want: 403},
		{name: "token model restriction", grant: true, limited: true, want: 403},
		{name: "database unavailable", grant: true, dbFailure: true, want: 503},
		{name: "old version", grant: true, version: "3.9.9", want: 400},
		{name: "prerelease", grant: true, version: "4.0.0-rc.1", want: 400},
		{name: "malformed version", grant: true, version: "4.0", want: 400},
		{name: "version prefix", grant: true, version: "v4.0.0", want: 400},
		{name: "leading zeros", grant: true, version: "04.0.0", want: 400},
		{name: "missing capability", grant: true, capability: "other", want: 400},
		{name: "wrong path", grant: true, path: "/v1/messages", want: 400},
		{name: "negative id", grant: true, id: "-4301", want: 400},
		{name: "signed id", grant: true, id: "+4301", want: 400},
		{name: "zero id", grant: true, id: "0", want: 400},
		{name: "invalid runtime", grant: true, setting: `{"twork_runtime":"bogus"}`, want: 403},
		{name: "broken runtime", grant: true, setting: `{"twork_runtime":"codex",`, want: 403},
		{name: "legacy explicit route", grant: true, setting: `{}`, want: 403},
		{name: "conflicting admin suffix", grant: true, specific: true, want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupDistributorTokenAffinityDB(t)
			if tc.memoryOff {
				common.MemoryCacheEnabled = false
			}
			seedDistributorChannel(t, 4301, "gpt-5", 0)
			seedDistributorChannel(t, 4302, "other-model", 0)
			setting := tc.setting
			if setting == "" {
				setting = `{"twork_runtime":"codex"}`
			}
			require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4301).Update("setting", setting).Error)
			if tc.grant {
				require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 991, ModelId: "gpt-5", ChannelId: 4301}).Error)
			}
			if tc.disabled {
				require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4301).Update("status", 2).Error)
			}
			if tc.health {
				require.NoError(t, model.DB.Create(&model.ChannelModelDisabled{ChannelId: 4301, Model: "gpt-5", Source: "manual"}).Error)
			}
			if tc.dbFailure {
				require.NoError(t, model.DB.Migrator().DropTable(&model.TokenModelChannel{}))
			}
			path := tc.path
			if path == "" {
				path = "/v1/responses"
			}
			payload := requestRuntimeRoute(t, 991, "default", path, `{"model":"gpt-5"}`, tc.want, func(c *gin.Context) {
				id := tc.id
				if id == "" {
					id = "4301"
				}
				version := tc.version
				if version == "" {
					version = "4.0.0"
				}
				cap := tc.capability
				if cap == "" {
					cap = "other, model-routes-v1"
				}
				c.Request.Header.Set("X-Twork-Channel-Id", id)
				c.Request.Header.Set("X-Twork-Client-Version", version)
				c.Request.Header.Set("X-Twork-Client-Capabilities", cap)
				c.Set("role", 100)
				if tc.limitAllowed {
					c.Set("token_model_limit_enabled", true)
					c.Set("token_model_limit", map[string]bool{"gpt-5": true})
				}
				if tc.limited {
					c.Set("token_model_limit_enabled", true)
					c.Set("token_model_limit", map[string]bool{"other": true})
				}
				if tc.specific {
					common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "4302")
				}
			})
			if tc.want == http.StatusOK {
				assert.Equal(t, 4301, payload.ChannelID)
			}
		})
	}
}

func TestDistributeLegacyCannotUseDedicatedSpecificOrAffinityChannel(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	seedDistributorChannel(t, 4401, "gpt-5", 100)
	seedDistributorChannel(t, 4402, "gpt-5", 0)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4401).Update("setting", `{"twork_runtime":"codex"}`).Error)
	model.InitChannelCache()
	requestRuntimeRoute(t, 0, "default", "/v1/responses", `{"model":"gpt-5"}`, 403, func(c *gin.Context) { common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, "4401") })
	seedAffinityChannel(t, "gpt-5", "default", "runtime-affinity", 4401)
	assert.Equal(t, 4402, requestDistributedChannel(t, 0, "default", "runtime-affinity", 200))
}

// 拒绝路径的下游恒返回 200，以检测错误放行，不能回显期望的错误状态。
func requestRuntimeRoute(t *testing.T, tokenID int, group, path, body string, want int, setup func(*gin.Context)) distributedChannelPayload {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, group)
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, tokenID)
		if setup != nil {
			setup(c)
		}
	})
	router.Use(Distribute())
	router.POST(path, func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"channel_id": c.GetInt("channel_id")}) })
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	require.Equal(t, want, rec.Code, rec.Body.String())
	var result distributedChannelPayload
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &result))
	if want != http.StatusOK {
		assert.Zero(t, result.ChannelID)
	}
	return result
}

func TestExplicitRuntimeRouteRejectsAmbiguousModelBeforeRelay(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	seedDistributorChannel(t, 4501, "gpt-5", 0)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4501).Update("setting", `{"twork_runtime":"codex","pass_through_body_enabled":true}`).Error)
	require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 993, ModelId: "gpt-5", ChannelId: 4501}).Error)
	for _, body := range []string{
		`{"model":"gpt-5","model":"forbidden"}`,
		`{"model":"gpt-5","Model":"forbidden"}`,
		`{"Model":"forbidden","model":"gpt-5"}`,
		`{"model":"gpt-5","MODEL":"gpt-5"}`,
		`{"model":"gpt-5","model":"gpt-5"}`,
		`{"m\u006fdel":"gpt-5"}`,
		`{"model":null}`,
		`{"model":123}`,
		`[{"model":"gpt-5"}]`,
	} {
		t.Run(body, func(t *testing.T) {
			requestRuntimeRoute(t, 993, "default", "/v1/responses", body, 400, func(c *gin.Context) {
				c.Request.Header.Set("X-Twork-Channel-Id", "4501")
				c.Request.Header.Set("X-Twork-Client-Version", "4.0.0")
				c.Request.Header.Set("X-Twork-Client-Capabilities", "model-routes-v1")
			})
		})
	}
}

func TestExplicitRuntimeRoutePreservesPassthroughRequestBody(t *testing.T) {
	require.NoError(t, i18n.Init())
	setupDistributorTokenAffinityDB(t)
	seedDistributorChannel(t, 4601, "gpt-5", 0)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 4601).Update("setting", `{"twork_runtime":"codex","pass_through_body_enabled":true}`).Error)
	require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 994, ModelId: "gpt-5", ChannelId: 4601}).Error)
	body := `{ "model" : "gpt-5", "input": [{"model":"nested","text":"原始内容"}], "max_output_tokens": 0 }`
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("token_id", 994)
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	})
	router.Use(Distribute())
	reached := false
	router.POST("/v1/responses", func(c *gin.Context) {
		reached = true
		raw, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		assert.Equal(t, body, string(raw))
		var decoded struct {
			Model string `json:"model"`
		}
		require.NoError(t, common.Unmarshal(raw, &decoded))
		assert.Equal(t, "gpt-5", decoded.Model)
		settings, exists := common.GetContextKey(c, constant.ContextKeyChannelSetting)
		require.True(t, exists)
		assert.True(t, settings.(dto.ChannelSettings).PassThroughBodyEnabled)
		assert.Equal(t, 4601, c.GetInt("channel_id"))
		c.Status(200)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Twork-Channel-Id", "4601")
	req.Header.Set("X-Twork-Client-Version", "4.0.0")
	req.Header.Set("X-Twork-Client-Capabilities", "model-routes-v1")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	assert.Equal(t, 200, rec.Code, rec.Body.String())
	assert.True(t, reached)
}
