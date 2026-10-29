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

// 假上游与网关均使用本地 TCP，核对实际出站工具历史/stop 和下游 SSE。
func TestClaudeConversionLocalHTTP(t *testing.T) {
	require.NoError(t, i18n.Init())
	service.InitTokenEncoders()
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	for _, tc := range []struct {
		name                            string
		stream, roleFirst, delayedUsage bool
	}{
		{name: "tool history and mixed stop"},
		{name: "mixed reasoning in first chunk", stream: true},
		{name: "mixed reasoning after role chunk", stream: true, roleFirst: true},
		{name: "mixed finish before usage", stream: true, roleFirst: true, delayedUsage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			channels := setupRelaySoftCooldownChannels(t, "gpt-conversion-audit", 1, 0, 1)
			requests := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				requests <- body
				if tc.stream {
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.roleFirst {
						fmt.Fprint(w, "data: {\"id\":\"audit\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
					}
					if tc.delayedUsage {
						fmt.Fprint(w, "data: {\"id\":\"audit\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking audit\",\"content\":\"answer audit\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"id\":\"audit\",\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
					} else {
						fmt.Fprint(w, "data: {\"id\":\"audit\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"thinking audit\",\"content\":\"answer audit\"}}]}\n\ndata: {\"id\":\"audit\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n")
					}
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"audit","type":"message","role":"assistant","model":"gpt-conversion-audit","content":[{"type":"text","text":"tools accepted"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`)
			}))
			t.Cleanup(upstream.Close)
			channelType := constant.ChannelTypeAnthropic
			path, format := "/v1/chat/completions", types.RelayFormatOpenAI
			body := `{"model":"gpt-conversion-audit","max_tokens":64,"stop":["A",42,null,true,"B"],"messages":[{"role":"user","content":"lookup"},{"role":"assistant","content":"first","tool_calls":[{"id":"a","type":"function","function":{"name":"first","arguments":"{\"n\":1}"}}]},{"role":"assistant","content":"second","tool_calls":[{"id":"b","type":"function","function":{"name":"second","arguments":"{\"n\":2}"}}]},{"role":"tool","tool_call_id":"a","content":"result a"},{"role":"tool","tool_call_id":"b","content":"result b"}]}`
			if tc.stream {
				channelType = constant.ChannelTypeOpenAI
				path, format = "/v1/messages", types.RelayFormatClaude
				body = `{"model":"gpt-conversion-audit","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`
			}
			require.NoError(t, model.DB.Model(channels[0]).Updates(map[string]any{"type": channelType, "base_url": upstream.URL}).Error)
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
			router.POST(path, func(c *gin.Context) { Relay(c, format) })
			gateway := httptest.NewServer(router)
			t.Cleanup(gateway.Close)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, gateway.URL+path, strings.NewReader(body))
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")
			resp, err := gateway.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			response, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, 200, resp.StatusCode, string(response))
			select {
			case request := <-requests:
				if !tc.stream {
					var wire struct {
						Stop     []string `json:"stop_sequences"`
						Messages []struct {
							Role    string
							Content any
						}
					}
					require.NoError(t, common.Unmarshal(request, &wire))
					assert.Equal(t, []string{"A", "B"}, wire.Stop)
					require.Len(t, wire.Messages, 3)
					blocks := wire.Messages[1].Content.([]any)
					var ids []string
					for _, b := range blocks {
						block := b.(map[string]any)
						if block["type"] == "tool_use" {
							ids = append(ids, block["id"].(string))
						}
					}
					assert.Equal(t, []string{"a", "b"}, ids)
					results := wire.Messages[2].Content.([]any)
					require.Len(t, results, 2)
					assert.Equal(t, "a", results[0].(map[string]any)["tool_use_id"])
					assert.Equal(t, "b", results[1].(map[string]any)["tool_use_id"])
					assert.Contains(t, string(response), "tools accepted")
				}
			default:
				t.Fatal("网关没有调用本地上游")
			}
			if tc.stream {
				var deltas []string
				var stops int
				for _, line := range strings.Split(string(response), "\n") {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					var event dto.ClaudeResponse
					require.NoError(t, common.UnmarshalJsonStr(strings.TrimPrefix(line, "data: "), &event))
					if event.Type == "content_block_delta" {
						if event.Delta.Thinking != nil {
							deltas = append(deltas, *event.Delta.Thinking)
						}
						if event.Delta.Text != nil {
							deltas = append(deltas, *event.Delta.Text)
						}
					}
					if event.Type == "message_stop" {
						stops++
					}
					if event.Type == "message_delta" {
						require.NotNil(t, event.Usage)
						assert.Equal(t, 7, event.Usage.InputTokens)
						assert.Equal(t, 3, event.Usage.OutputTokens)
					}
				}
				assert.Equal(t, []string{"thinking audit", "answer audit"}, deltas)
				assert.Equal(t, 1, stops)
			}
		})
	}
}
