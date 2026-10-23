package controller

import (
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
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 2026-10-23 负责人确认的换渠道规则：任何渠道故障都换、先冷却再升级禁用、渠道都失败才回统一繁忙提示。

type upstreamReply struct {
	status int
	stream bool
	body   string
}

const chatOKBody = `{"id":"chatcmpl-ok","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"备用渠道成功"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

const chatOKStream = "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"备用渠道\"}}]}\n\n" +
	"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"成功\"},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: [DONE]\n\n"

func useOpenAIChannels(t *testing.T, channels []*model.Channel, autoBan bool) {
	t.Helper()
	ban := 0
	if autoBan {
		ban = 1
	}
	for _, channel := range channels {
		channel.Type = constant.ChannelTypeOpenAI
		channel.AutoBan = &ban
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Updates(map[string]any{"type": constant.ChannelTypeOpenAI, "auto_ban": ban}).Error)
	}
	model.InitChannelCache()
}

func withAutoDisable(t *testing.T) {
	t.Helper()
	require.NoError(t, model.DB.AutoMigrate(&model.ChannelModelHealth{}))
	original := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalModelDisable := healthSetting.ModelLevelAutoDisableEnabled
	healthSetting.ModelLevelAutoDisableEnabled = true
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = original
		healthSetting.ModelLevelAutoDisableEnabled = originalModelDisable
	})
}

// runOpenAIRelay 按渠道域名给出上游回复，返回客户端收到的响应与依次访问的渠道域名。
func runOpenAIRelay(t *testing.T, modelName string, firstChannel *model.Channel, stream bool, reply func(host string) upstreamReply) (*httptest.ResponseRecorder, []string) {
	t.Helper()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	httpClient := service.GetHttpClient()
	originalTransport := httpClient.Transport
	var hosts []string
	httpClient.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		r := reply(req.URL.Host)
		contentType := "application/json"
		if r.stream {
			contentType = "text/event-stream"
		}
		return &http.Response{StatusCode: r.status, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(r.body)), Request: req}, nil
	})
	t.Cleanup(func() { httpClient.Transport = originalTransport })

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(fmt.Sprintf(
		`{"model":"%s","stream":%t,"messages":[{"role":"user","content":"hello"}]}`, modelName, stream)))
	c.Request.Header.Set("Content-Type", "application/json")
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
	require.Nil(t, middleware.SetupContextForSelectedChannel(c, firstChannel, modelName))
	Relay(c, types.RelayFormatOpenAI)
	return w, hosts
}

func TestRelayRetriesAnyChannelFailureAndCoolsItForChatCompletions(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("chat-any-failure")
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	useOpenAIChannels(t, channels, false)

	w, hosts := runOpenAIRelay(t, modelName, channels[0], false, func(host string) upstreamReply {
		if host == "channel-9700.test" {
			return upstreamReply{status: http.StatusBadGateway, body: `{"error":{"message":"The upstream service could not complete the request.","type":"upstream_error"}}`}
		}
		return upstreamReply{status: http.StatusOK, body: chatOKBody}
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "备用渠道成功")
	assert.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, hosts)
	// 坏渠道冷却：其他用户这段时间直接绕开它。
	_, cooling := service.GetChannelSoftCooldown(nil, channels[0].Id, modelName)
	assert.True(t, cooling)
}

func TestRelayRetriesHTTP200ErrorBody(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("chat-200-error")
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	useOpenAIChannels(t, channels, false)

	w, hosts := runOpenAIRelay(t, modelName, channels[0], false, func(host string) upstreamReply {
		if host == "channel-9700.test" {
			return upstreamReply{status: http.StatusOK, body: `{"error":{"message":"upstream exploded","type":"server_error"}}`}
		}
		return upstreamReply{status: http.StatusOK, body: chatOKBody}
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), "备用渠道成功")
	assert.Len(t, hosts, 2)
}

func TestRelayRetriesChatStreamWhoseFirstChunkIsAnError(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("chat-stream-first-error")
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	useOpenAIChannels(t, channels, false)

	w, hosts := runOpenAIRelay(t, modelName, channels[0], true, func(host string) upstreamReply {
		if host == "channel-9700.test" {
			return upstreamReply{status: http.StatusOK, stream: true, body: "data: {\"error\":{\"message\":\"upstream overloaded\",\"type\":\"server_error\"}}\n\n"}
		}
		return upstreamReply{status: http.StatusOK, stream: true, body: chatOKStream}
	})

	assert.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, hosts)
	assert.Contains(t, w.Body.String(), "备用渠道")
	assert.NotContains(t, w.Body.String(), "upstream overloaded")
}

func TestRelayCoolsChannelWhenChatStreamFailsAfterOutput(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("chat-stream-late-error")
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	useOpenAIChannels(t, channels, false)
	partial := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"写了一半\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"upstream dropped\",\"type\":\"server_error\"}}\n\n"

	w, hosts := runOpenAIRelay(t, modelName, channels[0], true, func(string) upstreamReply {
		return upstreamReply{status: http.StatusOK, stream: true, body: partial}
	})

	// 已写出正文不能重来，只试一个渠道；但这个渠道要按故障冷却。
	assert.Equal(t, []string{"channel-9700.test"}, hosts)
	assert.Contains(t, w.Body.String(), "写了一半")
	_, cooling := service.GetChannelSoftCooldown(nil, channels[0].Id, modelName)
	assert.True(t, cooling)
}

func TestRelayAllChannelsFailReturnsFriendlyBusyWithoutClientRetry(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("claude-all-fail")
	channels := setupRelaySoftCooldownChannels(t, modelName, 3, 3, 5)

	w, attempts, _ := runClaudeRelayWithStatuses(t, modelName, channels[0], []int{http.StatusInternalServerError})

	assert.Equal(t, 3, attempts)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, "false", w.Header().Get("X-Should-Retry"))
	// Claude 回包的 type 也是 upstream_unavailable，旧客户端与下一级网关都能识别。
	assert.Contains(t, w.Body.String(), `"type":"upstream_unavailable"`)
	assert.Contains(t, w.Body.String(), service.UpstreamUnavailableMessage)
	assert.NotContains(t, w.Body.String(), "upstream internal failure")
}

func TestRelaySkipsChannelWithBrokenSetupAndTriesNext(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("chat-setup-broken")
	channels := setupRelaySoftCooldownChannels(t, modelName, 3, 3, 5)
	useOpenAIChannels(t, channels, false)
	// 第二个渠道没有可用密钥：跳过它，健康的第三个渠道照样试。
	var broken model.Channel
	require.NoError(t, model.DB.First(&broken, channels[1].Id).Error)
	broken.Key = ""
	broken.ChannelInfo.IsMultiKey = true
	require.NoError(t, model.DB.Save(&broken).Error)
	model.InitChannelCache()

	w, hosts := runOpenAIRelay(t, modelName, channels[0], false, func(host string) upstreamReply {
		if host == "channel-9700.test" {
			return upstreamReply{status: http.StatusBadGateway, body: `{"error":{"message":"bad gateway","type":"upstream_error"}}`}
		}
		return upstreamReply{status: http.StatusOK, body: chatOKBody}
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"channel-9700.test", "channel-9702.test"}, hosts)
}

func TestRelayHardChannelFailureDisablesModelImmediately(t *testing.T) {
	modelName := uniqueRelaySoftCooldownModelName("chat-hard-failure")
	channels := setupRelaySoftCooldownChannels(t, modelName, 2, 3, 5)
	useOpenAIChannels(t, channels, true)
	withAutoDisable(t)

	w, hosts := runOpenAIRelay(t, modelName, channels[0], false, func(host string) upstreamReply {
		if host == "channel-9700.test" {
			return upstreamReply{status: http.StatusNotFound, body: fmt.Sprintf(`{"error":{"message":"Model \"%s\" is not available for this group","type":"new_api_error","code":"model_not_found"}}`, modelName)}
		}
		return upstreamReply{status: http.StatusOK, body: chatOKBody}
	})

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Len(t, hosts, 2)
	require.Eventually(t, func() bool {
		disabled, err := model.IsChannelModelDisabled(channels[0].Id, modelName)
		return err == nil && disabled
	}, 2*time.Second, 10*time.Millisecond)
}

func TestShouldDisableAfterRelayFailureEscalatesOnlyAfterRepeatedCooldowns(t *testing.T) {
	healthSetting := operation_setting.GetChannelHealthSetting()
	originalSetting := *healthSetting
	original := common.AutomaticDisableChannelEnabled
	t.Cleanup(func() {
		*healthSetting = originalSetting
		common.AutomaticDisableChannelEnabled = original
	})
	healthSetting.SoftFailureCooldownEnabled = true
	common.AutomaticDisableChannelEnabled = true
	serverErr := types.WithOpenAIError(types.OpenAIError{Message: "internal error", Type: "server_error"}, http.StatusInternalServerError)
	authErr := types.WithOpenAIError(types.OpenAIError{Message: "invalid api key", Type: "authentication_error", Code: "invalid_api_key"}, http.StatusUnauthorized)
	requestErr := types.WithOpenAIError(types.OpenAIError{Message: "prompt is too long", Type: "invalid_request_error"}, http.StatusBadRequest)
	c := newRetryTestContext()

	// 一次 500 只冷却，不禁用；连续冷却 3 次才禁用。
	assert.False(t, shouldDisableAfterRelayFailure(c, serverErr, 1))
	assert.False(t, shouldDisableAfterRelayFailure(c, serverErr, 2))
	assert.True(t, shouldDisableAfterRelayFailure(c, serverErr, 3))
	// 明确坏掉的直接禁用；请求本身的问题从不禁用渠道。
	assert.True(t, shouldDisableAfterRelayFailure(c, authErr, 1))
	assert.False(t, shouldDisableAfterRelayFailure(c, requestErr, 5))
}
