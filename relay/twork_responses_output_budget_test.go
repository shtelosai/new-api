package relay

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 核对真正收到 HTTP 请求的上游边界，避免只检查转换前 DTO。
func TestTworkResponsesOutputBudgetReachesOpenAIUpstream(t *testing.T) {
	service.InitHttpClient()
	for _, passThrough := range []bool{false, true} {
		for _, budget := range []int{0, 16, 131072} {
			t.Run(fmt.Sprintf("passthrough=%t/budget=%d", passThrough, budget), func(t *testing.T) {
				received := make(chan []byte, 1)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					assert.NoError(t, err)
					assert.Equal(t, "/v1/responses", r.URL.Path)
					assert.Equal(t, http.MethodPost, r.Method)
					received <- body
					w.WriteHeader(http.StatusNoContent)
				}))
				t.Cleanup(upstream.Close)
				body := map[string]any{"model": "public-gpt", "input": "验收", "reasoning": map[string]any{"effort": "medium"}}
				if budget != 0 {
					body["max_output_tokens"] = budget
				}
				raw, err := common.Marshal(body)
				require.NoError(t, err)
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
				info := &relaycommon.RelayInfo{
					OriginModelName: "public-gpt", RequestURLPath: "/v1/responses", RelayMode: relayconstant.RelayModeResponses,
					ChannelMeta: &relaycommon.ChannelMeta{
						ApiType: constant.APITypeOpenAI, ChannelType: constant.ChannelTypeOpenAI,
						ChannelBaseUrl: upstream.URL, UpstreamModelName: "gpt-6-astra", ApiKey: "test-only",
						ChannelSetting: dto.ChannelSettings{TworkRuntime: "pi", TworkWireAPI: "responses", PassThroughBodyEnabled: passThrough},
					},
				}
				adaptor := GetAdaptor(constant.APITypeOpenAI)
				adaptor.Init(info)
				converted, err := buildTworkModelUpstreamRequest(c, info, adaptor)
				require.NoError(t, err)
				response, err := adaptor.DoRequest(c, info, bytes.NewReader(converted))
				require.NoError(t, err)
				httpResponse, ok := response.(*http.Response)
				require.True(t, ok)
				t.Cleanup(func() { _ = httpResponse.Body.Close() })
				require.Equal(t, http.StatusNoContent, httpResponse.StatusCode)
				var actual map[string]any
				require.NoError(t, common.Unmarshal(<-received, &actual))
				assert.Equal(t, "gpt-6-astra", actual["model"])
				assert.Equal(t, "验收", actual["input"])
				assert.Equal(t, map[string]any{"effort": "medium"}, actual["reasoning"])
				assert.Equal(t, false, actual["store"])
				if budget == 0 {
					assert.NotContains(t, actual, "max_output_tokens")
				} else {
					assert.Equal(t, float64(budget), actual["max_output_tokens"])
				}
				assert.NotContains(t, actual, "max_tokens")
				assert.NotContains(t, actual, "max_completion_tokens")
			})
		}
	}
}
