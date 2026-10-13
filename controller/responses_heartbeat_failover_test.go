package controller

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesConcurrencyFailureAfterHeartbeat(t *testing.T) {
	for _, tc := range []struct {
		name        string
		prefix      string
		allFail     bool
		wantCalls   int
		wantSuccess bool
	}{
		{name: "只有心跳时切换", wantCalls: 2, wantSuccess: true},
		{name: "心跳和控制事件时切换", prefix: `data: {"type":"response.created","response":{"id":"discard-created"}}` + "\n\n", wantCalls: 2, wantSuccess: true},
		{name: "空正文增量仍允许切换", prefix: `data: {"type":"response.output_text.delta","delta":""}` + "\n\n", wantCalls: 2, wantSuccess: true},
		{name: "正文输出后不重复请求", prefix: `data: {"type":"response.output_text.delta","delta":"已输出正文"}` + "\n\n", wantCalls: 1},
		{name: "工具调用输出后不重复请求", prefix: `data: {"type":"response.output_item.added","item":{"id":"fc_test","type":"function_call","name":"read_note","call_id":"call_test","arguments":""}}` + "\n\n", wantCalls: 1},
		{name: "全部超并发时保留最后错误", allFail: true, wantCalls: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requestContext *gin.Context
			var hosts []string
			_, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				hosts = append(hosts, req.URL.Host)
				body := `data: {"type":"response.output_text.delta","delta":"备用渠道成功"}` + "\n\n" +
					`data: {"type":"response.completed","response":{"status":"completed"}}` + "\n\n"
				if len(hosts) == 1 || tc.allFail {
					// 确定性地重现请求等待阶段已写出的真实 SSE 保活帧。
					require.NoError(t, helper.PingData(requestContext))
					body = tc.prefix + `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for user, please retry later"}}}` + "\n\n"
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			require.NoError(t, model.DB.AutoMigrate(&model.User{}))
			router := gin.New()
			router.Use(func(c *gin.Context) {
				requestContext = c
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenId, 8701)
				common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
			})
			router.Use(middleware.Distribute())
			router.POST("/v1/responses", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIResponses) })

			w := requestResponsesFailover(router, name, "heartbeat-"+tc.name, true)
			require.Equal(t, http.StatusOK, w.Code)
			require.Len(t, hosts, tc.wantCalls, w.Body.String())
			assert.Equal(t, "channel-9700.test", hosts[0])
			if tc.wantCalls > 1 {
				assert.Equal(t, "channel-9701.test", hosts[1])
			}
			assert.Contains(t, w.Body.String(), ": PING\n\n")
			assert.NotContains(t, w.Body.String(), "discard-created")
			if tc.wantSuccess {
				assert.Contains(t, w.Body.String(), "备用渠道成功")
				assert.Contains(t, w.Body.String(), "response.completed")
				assert.NotContains(t, w.Body.String(), "gateway_concurrency_limit")
			} else {
				assert.NotContains(t, w.Body.String(), "备用渠道成功")
				assert.Contains(t, w.Body.String(), "gateway_concurrency_limit")
				assert.Contains(t, w.Body.String(), `data: {"code":`)
			}
		})
	}
}

func TestResponsesHeartbeatFailoverOverHTTP(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prefix    string
		wantCalls int32
	}{
		{name: "只有心跳时切换", wantCalls: 1},
		{name: "正文已输出时禁止切换", prefix: `data: {"type":"response.output_text.delta","delta":"已输出正文"}` + "\n\n"},
		{name: "工具调用已输出时禁止切换", prefix: `data: {"type":"response.function_call_arguments.delta","delta":"{}","item_id":"fc_test"}` + "\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			general := operation_setting.GetGeneralSetting()
			previous := *general
			general.PingIntervalEnabled, general.PingIntervalSeconds = true, 1
			t.Cleanup(func() { *general = previous })
			releaseUpstream := make(chan struct{})
			var firstCalls, backupCalls atomic.Int32
			first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				firstCalls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				// 上游保持排队，直到客户端真的收到网关定时保活。
				select {
				case <-releaseUpstream:
				case <-r.Context().Done():
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tc.prefix+`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"gateway_concurrency_limit","message":"Concurrency limit exceeded for user, please retry later"}}}`+"\n\n")
			}))
			t.Cleanup(first.Close)
			backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				backupCalls.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, `data: {"type":"response.output_text.delta","delta":"备用渠道成功"}`+"\n\n"+`data: {"type":"response.completed","response":{"status":"completed"}}`+"\n\n")
			}))
			t.Cleanup(backup.Close)
			router, name, channels := setupResponsesFailoverTest(t, http.DefaultTransport)
			require.NoError(t, model.DB.AutoMigrate(&model.User{}))
			require.NoError(t, model.DB.Model(channels[0]).Update("base_url", first.URL).Error)
			require.NoError(t, model.DB.Model(channels[1]).Update("base_url", backup.URL).Error)
			model.InitChannelCache()
			gateway := httptest.NewServer(router)
			t.Cleanup(gateway.Close)
			req, err := http.NewRequest(http.MethodPost, gateway.URL+"/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","store":false,"stream":true}`, name)))
			require.NoError(t, err)
			for key, value := range map[string]string{"Content-Type": "application/json", "X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3", "X-Twork-Route-Mode": "model", "X-Twork-Agent-Runtime": "pi", "X-Twork-Wire-Api": "responses"} {
				req.Header.Set(key, value)
			}
			client := &http.Client{Timeout: 10 * time.Second}
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			ping, err := reader.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, ": PING\n", ping)
			close(releaseUpstream)
			remaining, err := io.ReadAll(reader)
			require.NoError(t, err)
			assert.Equal(t, int32(1), firstCalls.Load())
			assert.Equal(t, tc.wantCalls, backupCalls.Load(), string(remaining))
			if tc.wantCalls > 0 {
				assert.Contains(t, string(remaining), "备用渠道成功")
				assert.Contains(t, string(remaining), "response.completed")
				assert.NotContains(t, string(remaining), "gateway_concurrency_limit")
			} else {
				assert.Contains(t, string(remaining), "gateway_concurrency_limit")
				assert.NotContains(t, string(remaining), "备用渠道成功")
			}
		})
	}
}

func TestResponsesHeartbeatRetryHonorsBoundaries(t *testing.T) {
	previous := *operation_setting.GetChannelHealthSetting()
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = true
	t.Cleanup(func() { *operation_setting.GetChannelHealthSetting() = previous })
	for _, tc := range []struct {
		name      string
		wantRetry bool
	}{
		{name: "只有心跳", wantRetry: true},
		{name: "已经输出正文"},
		{name: "指定渠道"},
		{name: "令牌指定渠道"},
		{name: "显式渠道"},
		{name: "压缩请求"},
		{name: "客户端取消"},
		{name: "重试预算耗尽"},
		{name: "旧版有状态请求"},
		{name: "鉴权错误沿用原有重试配置", wantRetry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyTworkRoutePolicy, model.TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"})
			require.NoError(t, helper.PingData(c))
			err := types.WithOpenAIError(types.OpenAIError{Type: "rate_limit_error", Code: "gateway_concurrency_limit", Message: "Concurrency limit exceeded for user, please retry later"}, http.StatusTooManyRequests)
			budget := 2
			switch tc.name {
			case "已经输出正文":
				require.NoError(t, helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.output_text.delta"}, `{"type":"response.output_text.delta","delta":"正文"}`))
			case "指定渠道":
				c.Set("specific_channel_id", 181)
			case "令牌指定渠道":
				common.SetContextKey(c, constant.ContextKeyTokenSpecificChannelId, 181)
			case "显式渠道":
				common.SetContextKey(c, constant.ContextKeyTworkExplicitChannelRoute, true)
			case "压缩请求":
				c.Request.URL.Path = "/v1/responses/compact"
			case "客户端取消":
				ctx, cancel := context.WithCancel(c.Request.Context())
				cancel()
				c.Request = c.Request.WithContext(ctx)
			case "重试预算耗尽":
				budget = 0
			case "旧版有状态请求":
				common.SetContextKey(c, constant.ContextKeyTworkRoutePolicy, model.TworkRoutePolicy{})
			case "鉴权错误沿用原有重试配置":
				// 现有状态码配置允许重试 401；心跳不能改变这项已有策略。
				err = types.WithOpenAIError(types.OpenAIError{Type: "authentication_error", Code: "invalid_api_key"}, http.StatusUnauthorized)
			}
			assert.Equal(t, tc.wantRetry, shouldRetry(c, err, budget))
		})
	}
}

func TestResponsesHeartbeatDoesNotClaimSemanticCommitInLogs(t *testing.T) {
	previous := *operation_setting.GetChannelHealthSetting()
	previousRedis := common.RedisEnabled
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = true
	common.RedisEnabled = false
	t.Cleanup(func() {
		*operation_setting.GetChannelHealthSetting() = previous
		common.RedisEnabled = previousRedis
	})
	for _, output := range []bool{false, true} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		common.SetContextKey(c, constant.ContextKeyTworkRoutePolicy, model.TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"})
		require.NoError(t, helper.PingData(c))
		if output {
			require.NoError(t, helper.ResponseChunkData(c, dto.ResponsesStreamResponse{Type: "response.output_text.delta"}, `{"type":"response.output_text.delta","delta":"正文"}`))
		}
		service.RecordChannelSoftCooldown(c, 9781, "heartbeat-log-test", http.StatusTooManyRequests, "rate_limit_error")
		adminInfo := map[string]interface{}{}
		service.AppendChannelSoftCooldownAdminInfo(c, adminInfo)
		info, ok := adminInfo["soft_cooldown"].(map[string]interface{})
		require.True(t, ok)
		assert.Equal(t, output, info["stream_semantic_committed"])
	}
}
