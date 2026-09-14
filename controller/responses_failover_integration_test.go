package controller

import (
	"fmt"
	"github.com/bytedance/gopkg/util/gopool"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupResponsesFailoverTest(t *testing.T, transport http.RoundTripper) (*gin.Engine, string, []*model.Channel) {
	t.Helper()
	require.NoError(t, i18n.Init())
	service.InitTokenEncoders()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	name := uniqueRelaySoftCooldownModelName("gpt-responses-failover")
	channels := setupRelaySoftCooldownChannels(t, name, 3, 2, 5)
	require.NoError(t, model.DB.AutoMigrate(&model.TokenModelChannel{}))
	for _, channel := range channels {
		require.NoError(t, model.DB.Model(channel).Updates(map[string]any{
			"type":    constant.ChannelTypeOpenAI,
			"setting": `{"twork_runtime":"pi","twork_wire_api":"responses","pass_through_body_enabled":true}`,
		}).Error)
		require.NoError(t, model.DB.Create(&model.TokenModelChannel{TokenId: 8701, ModelId: name, ChannelId: channel.Id}).Error)
	}
	model.InitChannelCache()
	affinity := operation_setting.GetChannelAffinitySetting()
	previous := *affinity
	t.Cleanup(func() { *affinity = previous })
	affinity.Enabled = true
	affinity.SwitchOnSuccess = true
	affinity.Rules = []operation_setting.ChannelAffinityRule{{Name: "codex cli trace", ModelRegex: []string{"^gpt-.*$"}, PathRegex: []string{"/v1/responses"}, KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "gjson", Path: "prompt_cache_key"}}, IncludeModelName: true, SkipRetryOnFailure: true}}
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previousTransport := client.Transport
	client.Transport = transport
	t.Cleanup(func() { client.Transport = previousTransport })
	router := gin.New()
	router.Use(func(c *gin.Context) {
		common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
		common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
		common.SetContextKey(c, constant.ContextKeyTokenId, 8701)
		common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	})
	router.Use(middleware.Distribute())
	router.POST("/v1/responses", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAIResponses) })
	return router, name, channels
}

func requestResponsesFailover(router *gin.Engine, name, session string, stream bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","store":false,"prompt_cache_key":%q,"stream":%t}`, name, session, stream)))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range map[string]string{"X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3", "X-Twork-Route-Mode": "model", "X-Twork-Agent-Runtime": "pi", "X-Twork-Wire-Api": "responses"} {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func TestResponsesFailoverRebindsAndSkipsCoolingChannels(t *testing.T) {
	var hosts []string
	broken := false
	router, name, channels := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		status, body := 200, `{"id":"ok","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
		if req.URL.Host == "channel-9700.test" && broken {
			status, body = 503, `{"error":{"type":"server_error","message":"upstream service error"}}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	w := requestResponsesFailover(router, name, "session-a", false)
	require.Equal(t, 200, w.Code)
	require.Equal(t, []string{"channel-9700.test"}, hosts)
	broken = true
	hosts = nil
	w = requestResponsesFailover(router, name, "session-a", false)
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, hosts)
	_, cooling := service.GetChannelSoftCooldown(nil, channels[0].Id, name)
	require.True(t, cooling)
	for _, session := range []string{"session-a", "session-b"} {
		hosts = nil
		w = requestResponsesFailover(router, name, session, false)
		require.Equal(t, 200, w.Code, w.Body.String())
		assert.Equal(t, []string{"channel-9701.test"}, hosts)
	}
	// 移除冷却过滤后最高优先级重新可选，已有会话仍应保持成功的备用渠道。
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = false
	broken = false
	hosts = nil
	require.Equal(t, 200, requestResponsesFailover(router, name, "session-a", false).Code)
	assert.Equal(t, []string{"channel-9701.test"}, hosts)
}

func TestResponsesFailedStreamDoesNotBindOrAppendAnotherReply(t *testing.T) {
	for _, terminal := range []string{`data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"upstream service error"}}}` + "\n\n", ""} {
		t.Run(fmt.Sprintf("terminal-%t", terminal != ""), func(t *testing.T) {
			var hosts []string
			router, name, channels := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				hosts = append(hosts, req.URL.Host)
				body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" + terminal
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			w := requestResponsesFailover(router, name, "stream-a", true)
			assert.Equal(t, []string{"channel-9700.test"}, hosts)
			assert.Contains(t, w.Body.String(), "partial")
			assert.Contains(t, w.Body.String(), `"type":"error"`)
			assert.NotContains(t, w.Body.String(), "}{")
			_, cooling := service.GetChannelSoftCooldown(nil, channels[0].Id, name)
			assert.True(t, cooling)
			hosts = nil
			requestResponsesFailover(router, name, "stream-a", true)
			assert.Equal(t, []string{"channel-9701.test"}, hosts)
		})
	}
}

func TestResponsesPreOutputFailuresTryRemainingPriorities(t *testing.T) {
	for _, failure := range []string{"http", "connect", "failed-json", "failed-stream", "truncated-stream"} {
		t.Run(failure, func(t *testing.T) {
			var hosts []string
			router, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				hosts = append(hosts, req.URL.Host)
				stream := strings.HasSuffix(failure, "stream")
				status, contentType := 200, "application/json"
				body := `{"status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
				if stream {
					contentType = "text/event-stream"
					body = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
				}
				if req.URL.Host != "channel-9702.test" {
					switch failure {
					case "http":
						status = 503
						body = `{"error":{"type":"server_error","message":"unavailable"}}`
					case "connect":
						return nil, fmt.Errorf("connect: connection refused")
					case "failed-json":
						body = `{"status":"failed","error":{"code":"server_error","message":"failed"}}`
					case "failed-stream":
						body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"discard-me\"}}\n\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"failed\"}\n\n"
					case "truncated-stream":
						body = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"discard-me\"}}\n\n"
					}
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}))
			w := requestResponsesFailover(router, name, "failover", strings.HasSuffix(failure, "stream"))
			require.Equal(t, 200, w.Code, w.Body.String())
			assert.Equal(t, []string{"channel-9700.test", "channel-9701.test", "channel-9702.test"}, hosts)
			assert.Contains(t, w.Body.String(), "completed")
			assert.NotContains(t, w.Body.String(), "discard-me")
		})
	}
}

func TestResponsesExhaustionPreservesLastErrorAndThenSkipsAllCooling(t *testing.T) {
	var hosts []string
	router, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"type":"server_error","message":%q}}`, "failed upstream "+strings.TrimSuffix(req.URL.Host, ".test")))), Request: req}, nil
	}))
	w := requestResponsesFailover(router, name, "exhausted", false)
	assert.Equal(t, 503, w.Code)
	assert.Contains(t, w.Body.String(), "failed upstream channel-9702")
	assert.Len(t, hosts, 3)
	hosts = nil
	w = requestResponsesFailover(router, name, "exhausted", false)
	assert.Equal(t, 503, w.Code)
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.Empty(t, hosts)
}

func TestResponsesTransientFailureDoesNotPersistDisable(t *testing.T) {
	router, name, channels := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		status, body := 200, `{"status":"completed","output":[]}`
		if req.URL.Host == "channel-9700.test" {
			status = 503
			body = `{"error":{"type":"server_error","message":"upstream service error"}}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	previousEnabled := common.AutomaticDisableChannelEnabled
	previousRanges := operation_setting.AutomaticDisableStatusCodeRanges
	common.AutomaticDisableChannelEnabled = true
	operation_setting.AutomaticDisableStatusCodeRanges = []operation_setting.StatusCodeRange{{Start: 503, End: 503}}
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = previousEnabled
		operation_setting.AutomaticDisableStatusCodeRanges = previousRanges
	})
	require.NoError(t, model.DB.Model(channels[0]).Update("auto_ban", 1).Error)
	model.InitChannelCache()
	require.Equal(t, 200, requestResponsesFailover(router, name, "disable", false).Code)
	require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, time.Second, 10*time.Millisecond)
	var count int64
	require.NoError(t, model.DB.Model(&model.ChannelModelDisabled{}).Where("channel_id = ? AND model = ?", channels[0].Id, name).Count(&count).Error)
	assert.Zero(t, count, "临时故障不能写入永久禁用表")
	_, cooling := service.GetChannelSoftCooldown(nil, channels[0].Id, name)
	assert.True(t, cooling)
}
