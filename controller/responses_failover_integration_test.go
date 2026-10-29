package controller

import (
	"context"
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
	// 全部失败后回统一友好文案，不透传最后一个渠道的原文。
	assert.Contains(t, w.Body.String(), service.UpstreamUnavailableMessage)
	assert.NotContains(t, w.Body.String(), "failed upstream")
	assert.Len(t, hosts, 3)
	hosts = nil
	// 全部在冷却：先放行最早到期的那一个试一次，还失败才回繁忙。
	w = requestResponsesFailover(router, name, "exhausted", false)
	assert.Equal(t, 503, w.Code)
	assert.Contains(t, w.Body.String(), service.UpstreamUnavailableMessage)
	assert.Len(t, hosts, 1)
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

// 读到结尾就关闭客户端连接，模拟桌面端读完 [DONE] 立即断开。
type closeClientAtEOF struct {
	io.Reader
	clientGone context.CancelFunc
}

func (b closeClientAtEOF) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		b.clientGone()
	}
	return n, err
}

func (closeClientAtEOF) Close() error { return nil }

func TestResponsesNonStreamProbeIsNotConfirmedAfterClientLeaves(t *testing.T) {
	var hosts []string
	broken := true
	var clientGone context.CancelFunc
	router, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		status, body := 200, `{"id":"ok","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
		if req.URL.Host == "channel-9700.test" && broken {
			status, body = 503, `{"error":{"type":"server_error","message":"upstream service error"}}`
		}
		var reader io.ReadCloser = io.NopCloser(strings.NewReader(body))
		if clientGone != nil {
			reader = closeClientAtEOF{Reader: strings.NewReader(body), clientGone: clientGone}
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: reader, Request: req}, nil
	}))
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownSeconds = 1
	require.Equal(t, 200, requestResponsesFailover(router, name, "probe-a", false).Code)
	require.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, hosts)
	broken = false
	time.Sleep(1100 * time.Millisecond)

	send := func(session string, disconnect bool) []string {
		hosts = nil
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","store":false,"prompt_cache_key":%q,"stream":false}`, name, session)))
		if disconnect {
			var ctx context.Context
			ctx, clientGone = context.WithCancel(context.Background())
			req = req.WithContext(ctx)
		}
		req.Header.Set("Content-Type", "application/json")
		for key, value := range map[string]string{"X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3", "X-Twork-Route-Mode": "model", "X-Twork-Agent-Runtime": "pi", "X-Twork-Wire-Api": "responses"} {
			req.Header.Set(key, value)
		}
		router.ServeHTTP(httptest.NewRecorder(), req)
		clientGone = nil
		return hosts
	}
	assert.Equal(t, []string{"channel-9700.test"}, send("probe-b", true), "冷却到期放一个请求试探")
	// 非流式没有统一完成标记，客户端已走时保守不确认：试探名额仍占着，新会话先走备用渠道。
	assert.Equal(t, []string{"channel-9701.test"}, send("probe-c", false), "客户端已走的非流式试探不算恢复")
}

// 客户端收到结束事件就断开：模拟桌面端读到 response.completed 立即关闭连接。
type leaveAfterCompleted struct {
	*httptest.ResponseRecorder
	clientGone context.CancelFunc
}

func (w leaveAfterCompleted) Flush() {
	w.ResponseRecorder.Flush()
	// 只有完整 data 事件已刷新，才等价于客户端读到终态后断开。
	if strings.Contains(w.Body.String(), `data: {"type":"response.completed"`) && strings.HasSuffix(w.Body.String(), "\n\n") {
		w.clientGone()
	}
}

func TestResponsesStreamClientDisconnectAfterEndKeepsRules(t *testing.T) {
	var hosts []string
	broken := true
	router, name, _ := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		if req.URL.Host == "channel-9700.test" && broken {
			return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"server_error","message":"upstream service error"}}`)), Request: req}, nil
		}
		body := `data: {"type":"response.output_text.delta","delta":"好"}` + "\n\n" + `data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownSeconds = 1
	send := func(session string) []string {
		hosts = nil
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","store":false,"prompt_cache_key":%q,"stream":true}`, name, session))).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		for key, value := range map[string]string{"X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3", "X-Twork-Route-Mode": "model", "X-Twork-Agent-Runtime": "pi", "X-Twork-Wire-Api": "responses"} {
			req.Header.Set(key, value)
		}
		w := leaveAfterCompleted{ResponseRecorder: httptest.NewRecorder(), clientGone: cancel}
		router.ServeHTTP(w, req)
		require.Contains(t, w.Body.String(), "response.completed", "%d %v %s", w.Code, hosts, w.Body.String())
		require.Error(t, ctx.Err(), "客户端在收到结束事件后已断开")
		return hosts
	}
	require.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, send("stay-on-b"))
	broken = false
	time.Sleep(1100 * time.Millisecond)
	assert.Equal(t, []string{"channel-9700.test"}, send("probe-stream"), "冷却到期放一个请求试探")
	assert.Equal(t, []string{"channel-9700.test"}, send("new-stream"), "试探成功后新会话回到最高优先级")
	assert.Equal(t, []string{"channel-9701.test"}, send("stay-on-b"), "换渠道成功后会话粘到新渠道，读完即断开也要记上")
}
