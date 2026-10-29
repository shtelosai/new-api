package controller

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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

// 通过真实分发、模型映射、渠道系统提示和本地 TCP 回执验证出站 JSON。
func TestGPT61LocalHTTPMapping(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitTokenEncoders()
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	for _, tc := range []struct {
		name, public, upstream, effort, wantModel, wantEffort string
		compatible, sampling, passthrough                     bool
	}{
		{"Sol alias", "sol-alias", "gpt-6.1-sol", "none", "gpt-6.1-sol", "none", true, false, false},
		{"Terra default", "terra-alias", "gpt-6.1-terra", "", "gpt-6.1-terra", "", true, false, false},
		{"old public Sol maps to Terra none", "gpt-5.6-sol", "gpt-6.1-terra-none", "high", "gpt-6.1-terra", "none", true, true, false},
		{"dated Terra high", "dated-alias", "gpt-6.1-terra-2026-10-20-high", "none", "gpt-6.1-terra-2026-10-20", "high", true, false, false},
		{"public GPT61 maps to Qwen", "gpt-6.1-terra", "qwen-turbo", "none", "qwen-turbo", "none", false, true, false},
		{"explicit body passthrough", "gpt-6.1-terra", "gpt-6.1-terra", "high", "gpt-6.1-terra", "high", false, true, true},
	} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				channels := setupRelaySoftCooldownChannels(t, tc.public, 1, 0, 1)
				requests := make(chan []byte, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					data, err := io.ReadAll(r.Body)
					if err != nil {
						http.Error(w, err.Error(), 400)
						return
					}
					requests <- data
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"id\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
					} else {
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"id":"test","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0}}`)
					}
				}))
				t.Cleanup(upstream.Close)
				mapping, err := common.Marshal(map[string]string{tc.public: tc.upstream})
				require.NoError(t, err)
				setting, err := common.Marshal(map[string]any{"pass_through_body_enabled": tc.passthrough, "system_prompt": "channel policy"})
				require.NoError(t, err)
				require.NoError(t, model.DB.Model(channels[0]).Updates(map[string]any{"type": constant.ChannelTypeOpenAI, "base_url": upstream.URL, "model_mapping": string(mapping), "setting": string(setting)}).Error)
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
				router.POST("/v1/chat/completions", func(c *gin.Context) { Relay(c, types.RelayFormatOpenAI) })
				gateway := httptest.NewServer(router)
				t.Cleanup(gateway.Close)
				request := &dto.GeneralOpenAIRequest{Model: tc.public, Stream: common.GetPointer(stream), ReasoningEffort: tc.effort, MaxTokens: common.GetPointer(uint(16)), Temperature: common.GetPointer(0.0), TopP: common.GetPointer(0.0), LogProbs: common.GetPointer(false), TopLogProbs: common.GetPointer(0), Messages: []dto.Message{{Role: "system", Content: "policy"}, {Role: "user", Content: "hello"}}}
				body, err := common.Marshal(request)
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, "POST", gateway.URL+"/v1/chat/completions", bytes.NewReader(body))
				require.NoError(t, err)
				req.Header.Set("Content-Type", "application/json")
				response, err := gateway.Client().Do(req)
				require.NoError(t, err)
				defer response.Body.Close()
				output, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.Equal(t, 200, response.StatusCode, string(output))
				assert.Contains(t, string(output), "ok")
				var wire map[string]any
				select {
				case data := <-requests:
					require.NoError(t, common.Unmarshal(data, &wire))
				default:
					t.Fatal("没有收到上游请求")
				}
				assert.Equal(t, tc.wantModel, wire["model"])
				if tc.wantEffort != "" {
					assert.Equal(t, tc.wantEffort, wire["reasoning_effort"])
				}
				role := "system"
				if tc.compatible {
					role = "developer"
					assert.NotContains(t, wire, "max_tokens")
					assert.Equal(t, float64(16), wire["max_completion_tokens"])
				} else {
					assert.Equal(t, float64(16), wire["max_tokens"])
				}
				assert.Equal(t, role, wire["messages"].([]any)[0].(map[string]any)["role"])
				for _, key := range []string{"temperature", "top_p", "logprobs", "top_logprobs"} {
					if tc.sampling {
						assert.Contains(t, wire, key)
					} else {
						assert.NotContains(t, wire, key)
					}
				}
			})
		}
	}
}
