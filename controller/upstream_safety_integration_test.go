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
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 使用临时 SQLite 和本地 TCP 上游，覆盖分发、协议转换和真实 HTTP 回执。
func TestUpstreamSafetyLocalHTTP(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitTokenEncoders()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, tc := range []struct {
		name   string
		path   string
		format types.RelayFormat
		body   string
	}{
		{name: "Claude tool precision", path: "/v1/messages", format: types.RelayFormatClaude,
			body: `{"model":"gpt-upstream-safety","max_tokens":64,"messages":[{"role":"user","content":"lookup"}]}`},
		{name: "Gemini text with tool", path: "/v1beta/models/gpt-upstream-safety:generateContent", format: types.RelayFormatGemini,
			body: `{"contents":[{"role":"model","parts":[{"text":"Checking now"},{"functionCall":{"name":"lookup","args":{"city":"Paris"}}}]},{"role":"user","parts":[{"text":"continue"}]}]}`},
		{name: "bare DONE before EOF", path: "/v1/chat/completions", format: types.RelayFormatOpenAI,
			body: `{"model":"gpt-upstream-safety","stream":true,"messages":[{"role":"user","content":"hello"}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channels := setupRelaySoftCooldownChannels(t, "gpt-upstream-safety", 1, 0, 1)
			requests := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				requests <- body
				if strings.Contains(tc.body, `"stream":true`) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"id\":\"stream\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"id\":\"stream\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n[DONE]\n\ndata: should_not_appear\n\n")
					w.(http.Flusher).Flush()
					// 不发送 EOF：网关应识别 DONE 并主动关闭上游连接。
					<-r.Context().Done()
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"tool","object":"chat.completion","model":"gpt-upstream-safety","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"id\":9007199254740993,\"value\":0.123456789012345678901}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
			}))
			t.Cleanup(upstream.Close)
			require.NoError(t, model.DB.Model(channels[0]).Updates(map[string]any{
				"type": constant.ChannelTypeOpenAI, "base_url": upstream.URL,
			}).Error)
			model.InitChannelCache()
			if service.GetHttpClient() == nil {
				service.InitHttpClient()
			}
			client := service.GetHttpClient()
			previous := client.Transport
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = nil
			client.Transport = transport
			t.Cleanup(func() { client.Transport = previous; transport.CloseIdleConnections() })

			router := gin.New()
			router.Use(func(c *gin.Context) {
				common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
				common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
				common.SetContextKey(c, constant.ContextKeyTokenId, 8701)
				common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{AcceptUnsetRatioModel: true})
			})
			router.Use(middleware.Distribute())
			router.POST(tc.path, func(c *gin.Context) { Relay(c, tc.format) })
			gateway := httptest.NewServer(router)
			t.Cleanup(gateway.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+tc.path, strings.NewReader(tc.body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			response, err := gateway.Client().Do(req)
			require.NoError(t, err)
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, response.StatusCode, string(body))
			select {
			case request := <-requests:
				if tc.format == types.RelayFormatGemini {
					var wire dto.GeneralOpenAIRequest
					require.NoError(t, common.Unmarshal(request, &wire))
					require.Len(t, wire.Messages, 2)
					assert.Equal(t, "Checking now", wire.Messages[0].StringContent())
					require.Len(t, wire.Messages[0].ParseToolCalls(), 1)
				}
			default:
				t.Fatal("网关未调用本地上游")
			}
			switch tc.format {
			case types.RelayFormatClaude:
				assert.Contains(t, string(body), `"id":9007199254740993`)
				assert.Contains(t, string(body), `"value":0.123456789012345678901`)
				assert.Contains(t, string(body), `"type":"tool_use"`)
			case types.RelayFormatOpenAI:
				assert.Contains(t, string(body), "hello")
				assert.Contains(t, string(body), "[DONE]")
				assert.NotContains(t, string(body), "should_not_appear")
			}
		})
	}
}
