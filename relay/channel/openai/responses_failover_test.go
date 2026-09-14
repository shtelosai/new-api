package openai

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesTerminalErrorsPreserveMeaning(t *testing.T) {
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"status":"completed"}`, 0},
		{`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}`, 0},
		{`{"status":"failed","error":{"code":"server_error","message":"upstream failed"}}`, 502},
		{`{"status":"failed","error":{"type":"invalid_request_error","message":"invalid input"}}`, 400},
		{`{"status":"failed","error":{"code":"insufficient_quota","message":"quota exceeded"}}`, 429},
		{`{"status":"in_progress"}`, 502},
	} {
		t.Run(tc.body, func(t *testing.T) {
			var response dto.OpenAIResponsesResponse
			require.NoError(t, common.UnmarshalJsonStr(tc.body, &response))
			err := responsesFailure(&response)
			if tc.status == 0 {
				assert.Nil(t, err)
			} else {
				require.NotNil(t, err)
				assert.Equal(t, tc.status, err.StatusCode)
			}
		})
	}
}

func TestResponsesFailedStreamReturnsReportedUsage(t *testing.T) {
	previous := *operation_setting.GetChannelHealthSetting()
	previousTimeout := constant.StreamingTimeout
	t.Cleanup(func() {
		*operation_setting.GetChannelHealthSetting() = previous
		constant.StreamingTimeout = previousTimeout
	})
	operation_setting.GetChannelHealthSetting().SoftFailureCooldownEnabled = true
	constant.StreamingTimeout = 30
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	common.SetContextKey(c, constant.ContextKeyTworkRoutePolicy, model.TworkRoutePolicy{ModelRoute: true, WireAPI: "responses"})
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"failed\"},\"usage\":{\"input_tokens\":100,\"output_tokens\":20,\"input_tokens_details\":{\"cached_tokens\":60}}}}\n\n"
	usage, err := OaiResponsesStreamHandler(c, &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-test"}}, &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body))})
	require.NotNil(t, err)
	require.NotNil(t, usage)
	assert.Equal(t, 100, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)
	assert.Equal(t, 120, usage.TotalTokens)
	assert.Equal(t, 60, usage.PromptTokensDetails.CachedTokens)
	assert.True(t, c.Writer.Written())
}
