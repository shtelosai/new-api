package controller

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTaskRetryBillingLocalHTTP(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		wallet, token, ratio, want int
		fail, reject               bool
	}{
		{"more expensive", 1000, 1000, 3, 300, false, false},
		{"cheaper", 1000, 1000, 0, 40, false, false},
		{"wallet insufficient", 200, 1000, 3, 0, false, true},
		{"token insufficient", 1000, 200, 3, 0, false, true},
		{"all failed refund", 1000, 1000, 3, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const name = "task-retry-audit"
			channels := setupRelaySoftCooldownChannels(t, name, 2, 1, 2)
			oldBatch := common.BatchUpdateEnabled
			common.BatchUpdateEnabled = false
			t.Cleanup(func() { common.BatchUpdateEnabled = oldBatch })
			common.LogConsumeEnabled = true
			t.Setenv("LOG_SQL_DSN", "")
			require.NoError(t, model.InitLogDB())
			require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Token{}, &model.Task{}, &model.Log{}))
			user := model.User{Id: 8710, Username: "task-retry", AffCode: "task-retry", Quota: tc.wallet}
			token := model.Token{Id: 8701, UserId: 8710, Key: "task-retry-key", Status: common.TokenStatusEnabled, ExpiredTime: -1, RemainQuota: tc.token}
			require.NoError(t, model.DB.Create(&user).Error)
			require.NoError(t, model.DB.Create(&token).Error)
			previous, err := common.Marshal(ratio_setting.GetModelPriceCopy())
			require.NoError(t, err)
			require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"task-retry-audit":0.00005}`))
			t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(string(previous))) })
			type observation struct {
				channel, wallet, token int
				model                  string
			}
			calls := make(chan observation, 4)
			for index, ch := range channels {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var body map[string]any
					data, _ := io.ReadAll(r.Body)
					_ = common.Unmarshal(data, &body)
					var u model.User
					var tok model.Token
					if err := model.DB.First(&u, user.Id).Error; err != nil {
						t.Error(err)
					}
					if err := model.DB.First(&tok, token.Id).Error; err != nil {
						t.Error(err)
					}
					calls <- observation{index, u.Quota, tok.RemainQuota, fmt.Sprint(body["model"])}
					w.Header().Set("Content-Type", "application/json")
					if index == 0 || tc.fail {
						w.WriteHeader(503)
						fmt.Fprint(w, `{"error":{"message":"retry test failure","type":"server_error"}}`)
						return
					}
					fmt.Fprint(w, `{"id":"upstream-video","object":"video","model":"task-retry-audit","status":"queued"}`)
				}))
				t.Cleanup(server.Close)
				ratio := 1.0
				if index == 1 {
					ratio = float64(tc.ratio)
					if tc.ratio == 0 {
						ratio = 0.4
					}
				}
				mapping := fmt.Sprintf(`{"task-retry-audit":"mapped-%d"}`, index)
				require.NoError(t, model.DB.Model(ch).Updates(map[string]any{"type": constant.ChannelTypeOpenAI, "base_url": server.URL, "ratio": ratio, "model_mapping": mapping}).Error)
			}
			model.InitChannelCache()
			if service.GetHttpClient() == nil {
				service.InitHttpClient()
			}
			client := service.GetHttpClient()
			oldTransport := client.Transport
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			client.Transport = transport
			t.Cleanup(func() { client.Transport = oldTransport; transport.CloseIdleConnections() })
			router := gin.New()
			router.Use(func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserId, user.Id)
				common.SetContextKey(c, constant.ContextKeyTokenId, token.Id)
				common.SetContextKey(c, constant.ContextKeyTokenKey, token.Key)
				common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only", QuotaWarningThreshold: -1})
			})
			router.Use(middleware.Distribute())
			router.POST("/v1/videos", RelayTask)
			gateway := httptest.NewServer(router)
			t.Cleanup(gateway.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+"/v1/videos", strings.NewReader(`{"model":"task-retry-audit","prompt":"test video","seconds":"4","size":"720x1280"}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := gateway.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			if tc.reject {
				assert.Equal(t, 403, resp.StatusCode, string(data))
			} else if tc.fail {
				assert.Equal(t, 503, resp.StatusCode, string(data))
			} else {
				assert.Equal(t, 200, resp.StatusCode, string(data))
			}
			require.Eventually(t, func() bool {
				var u model.User
				var tok model.Token
				return model.DB.First(&u, user.Id).Error == nil && model.DB.First(&tok, token.Id).Error == nil && u.Quota == tc.wallet-tc.want && tok.RemainQuota == tc.token-tc.want
			}, time.Second, time.Millisecond, "钱包与令牌最终扣费不符")
			require.NoError(t, model.DB.First(&user, user.Id).Error)
			require.NoError(t, model.DB.First(&token, token.Id).Error)
			assert.Equal(t, tc.want, user.UsedQuota)
			assert.Equal(t, tc.want, token.UsedQuota)
			var observed []observation
			for len(calls) > 0 {
				observed = append(observed, <-calls)
			}
			wantCalls := 2
			if tc.reject {
				wantCalls = 1
			}
			require.Len(t, observed, wantCalls, "余额不足时不得调用更贵的上游")
			assert.Equal(t, observation{0, tc.wallet - 100, tc.token - 100, "mapped-0"}, observed[0])
			if wantCalls == 2 {
				held := 300
				if tc.ratio == 0 {
					held = 100
				}
				assert.Equal(t, observation{1, tc.wallet - held, tc.token - held, "mapped-1"}, observed[1], "上游提交前必须完成差额预留")
			}
			var tasks []model.Task
			require.NoError(t, model.DB.Find(&tasks).Error)
			var logs []model.Log
			require.NoError(t, model.LOG_DB.Where("type = ?", model.LogTypeConsume).Find(&logs).Error)
			if tc.want == 0 {
				assert.Empty(t, tasks)
				assert.Empty(t, logs)
			} else {
				require.Len(t, tasks, 1)
				assert.Equal(t, tc.want, tasks[0].Quota)
				assert.Equal(t, channels[1].Id, tasks[0].ChannelId)
				require.Len(t, logs, 1)
				assert.Equal(t, tc.want, logs[0].Quota)
				require.NotNil(t, tasks[0].PrivateData.BillingContext.ChannelRatio)
				expectedRatio := 3.0
				if tc.ratio == 0 {
					expectedRatio = 0.4
				}
				assert.Equal(t, expectedRatio, *tasks[0].PrivateData.BillingContext.ChannelRatio)
			}
		})
	}
}
