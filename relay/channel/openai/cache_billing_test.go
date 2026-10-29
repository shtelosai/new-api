package openai

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesCacheBillingLocalHTTP(t *testing.T) {
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previous })
	const payload = `{"id":"cache","status":"completed","output":[],"usage":{"input_tokens":12000,"output_tokens":200,"total_tokens":12200,"input_tokens_details":{"cached_tokens":4000,"cache_write_tokens":7000,"cache_creation_input_tokens":7000,"cache_write_tokens_details":{"ttl_30m_tokens":3000,"ttl_12h_tokens":4000}}}}`
	for _, mode := range []string{"nonstream", "stream", "compact", "stream-to-chat", "stream-sparse", "stream-to-chat-sparse"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(mode, "stream") {
					w.Header().Set("Content-Type", "text/event-stream")
					final := payload
					if strings.HasSuffix(mode, "-sparse") {
						initial := strings.Replace(payload, `"status":"completed"`, `"status":"in_progress"`, 1)
						fmt.Fprintf(w, "data: {\"type\":\"response.in_progress\",\"response\":%s}\n\n", initial)
						final = strings.Replace(payload, `"cache_write_tokens":7000,"cache_creation_input_tokens":7000,"cache_write_tokens_details":{"ttl_30m_tokens":3000,"ttl_12h_tokens":4000}`, `"cache_write_tokens":0`, 1)
					}
					fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\ndata: [DONE]\n\n", final)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, payload)
			}))
			defer server.Close()
			response, err := server.Client().Get(server.URL)
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
			info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAI, ShouldIncludeUsage: true, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}
			var usage *dto.Usage
			var apiErr *types.NewAPIError
			switch mode {
			case "nonstream":
				usage, apiErr = OaiResponsesHandler(c, info, response)
			case "stream", "stream-sparse":
				usage, apiErr = OaiResponsesStreamHandler(c, info, response)
			case "compact":
				usage, apiErr = OaiResponsesCompactionHandler(c, response)
			case "stream-to-chat", "stream-to-chat-sparse":
				usage, apiErr = OaiResponsesToChatStreamHandler(c, info, response)
			}
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, 12000, usage.PromptTokens)
			assert.Equal(t, 7000, usage.PromptTokensDetails.CacheCreationTokensTotal())
			assert.Equal(t, 4000, usage.PromptTokensDetails.CacheWrite12hTokens())
			assert.Equal(t, 7000, usage.PromptTokensDetails.CacheCreationInputTokens)
		})
	}
}

func TestChatStreamCacheBillingKeepsEarlierWrites(t *testing.T) {
	body := "data: {\"id\":\"cache\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":12000,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":4000,\"cache_creation_input_tokens\":7000,\"cache_write_tokens_details\":{\"ttl_12h_tokens\":4000}}}}\n\ndata: {\"id\":\"cache\",\"model\":\"test\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12000,\"completion_tokens\":200,\"prompt_tokens_details\":{\"cached_tokens\":4000}}}\n\ndata: [DONE]\n\n"
	c, _, response, info := newResponsesChatTestContext(t, body, true)
	previous := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previous })
	usage, apiErr := OaiStreamHandler(c, info, response)
	require.Nil(t, apiErr)
	require.NotNil(t, usage)
	assert.Equal(t, 200, usage.CompletionTokens)
	assert.Equal(t, 7000, usage.PromptTokensDetails.CacheCreationTokensTotal())
	assert.Equal(t, 4000, usage.PromptTokensDetails.CacheWrite12hTokens())
}
