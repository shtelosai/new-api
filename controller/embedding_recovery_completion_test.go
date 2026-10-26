package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type cancelAfterEmbeddingWrite struct {
	gin.ResponseWriter
	cancel context.CancelFunc
}

func (w cancelAfterEmbeddingWrite) Write(data []byte) (int, error) {
	n, err := w.ResponseWriter.Write(data)
	w.cancel()
	return n, err
}

func TestEmbeddingRecoveryAfterValidatedBodyIsDelivered(t *testing.T) {
	const valid = `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2]}],"usage":{"prompt_tokens":2,"total_tokens":2}}`
	for _, tc := range []struct {
		name         string
		body         string
		mode         int
		cancelBefore bool
		broken       bool
		wantError    bool
		wantComplete bool
	}{
		{name: "complete embedding delivered before disconnect", body: valid, mode: relayconstant.RelayModeEmbeddings, wantComplete: true},
		{name: "client abandoned before response", body: valid, mode: relayconstant.RelayModeEmbeddings, cancelBefore: true},
		{name: "partial body", body: valid, mode: relayconstant.RelayModeEmbeddings, broken: true, wantError: true},
		{name: "invalid JSON", body: `{"data":`, mode: relayconstant.RelayModeEmbeddings, wantError: true},
		{name: "upstream error in 200", body: `{"error":{"type":"server_error","message":"unavailable"}}`, mode: relayconstant.RelayModeEmbeddings, wantError: true},
		{name: "other nonstream route keeps conservative behavior", body: chatOKBody, mode: relayconstant.RelayModeChatCompletions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil).WithContext(ctx)
			c.Writer = cancelAfterEmbeddingWrite{ResponseWriter: c.Writer, cancel: cancel}
			if tc.cancelBefore {
				cancel()
			}
			info := &relaycommon.RelayInfo{
				RelayMode: tc.mode, RelayFormat: types.RelayFormatOpenAI,
				ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "embedding-test"},
			}
			var body io.Reader = strings.NewReader(tc.body)
			if tc.broken {
				body = &brokenReader{data: body}
			}
			_, err := openai.OpenaiHandler(c, info, &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(body)})
			if tc.wantError {
				require.NotNil(t, err)
				cancel()
			} else {
				require.Nil(t, err)
			}
			assert.Equal(t, tc.wantComplete, relayUpstreamFinished(c, info))
			if tc.wantComplete {
				resetAttemptStreamState(c)
				assert.False(t, relayUpstreamFinished(c, info), "完成标记不能跨重试尝试沿用")
			}
		})
	}
}
