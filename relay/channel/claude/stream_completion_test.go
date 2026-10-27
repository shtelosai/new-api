package claude

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const completionPrefix = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_complete\",\"model\":\"claude-test\",\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"
const completionDelta = "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n"
const completionStop = "data: {\"type\":\"message_stop\"}\n\n"

// 模拟客户端在收到指定事件时立即关连接，稳定覆盖写出结束帧时的竞态。
type cancelOnClaudeEventWriter struct {
	gin.ResponseWriter
	event  string
	cancel context.CancelFunc
}

func (w *cancelOnClaudeEventWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	if strings.Contains(string(p), w.event) {
		w.cancel()
	}
	return n, err
}

func TestClaudeStreamCompletionPreservesCancellationBoundary(t *testing.T) {
	for _, tc := range []struct {
		name, body, cancelEvent string
		closeError              error
		reason                  relaycommon.StreamEndReason
	}{
		{name: "should succeed when client closes on message_stop", body: completionPrefix + completionDelta + completionStop, cancelEvent: "event: message_stop", reason: relaycommon.StreamEndReasonDone},
		{name: "should finish when upstream stays open after message_stop", body: completionPrefix + completionDelta + completionStop, reason: relaycommon.StreamEndReasonDone},
		{name: "should preserve cancellation before message_stop", body: completionPrefix + completionDelta + completionStop, cancelEvent: "event: content_block_delta", reason: relaycommon.StreamEndReasonClientGone},
		{name: "should not treat stop_reason as completion when client cancels", body: completionPrefix + completionDelta, cancelEvent: "event: message_delta", reason: relaycommon.StreamEndReasonClientGone},
		{name: "should preserve upstream error before message_stop", body: completionPrefix + "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n" + completionStop, reason: relaycommon.StreamEndReasonHandlerStop},
		{name: "should preserve upstream read failure before message_stop", body: completionPrefix, closeError: io.ErrUnexpectedEOF, reason: relaycommon.StreamEndReasonScannerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := newClaudeStreamTestContextWithRecorder(t)
			ctx, cancel := context.WithCancel(c.Request.Context())
			defer cancel()
			c.Request = c.Request.WithContext(ctx)
			if tc.cancelEvent != "" {
				c.Writer = &cancelOnClaudeEventWriter{ResponseWriter: c.Writer, event: tc.cancelEvent, cancel: cancel}
			}
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			writeDone := make(chan struct{})
			go func() {
				defer close(writeDone)
				_, _ = io.WriteString(writer, tc.body)
				if tc.closeError != nil {
					_ = writer.CloseWithError(tc.closeError)
				}
			}()
			info := newClaudeStreamRelayInfo()
			info.IsStream = true
			done := make(chan struct{})
			var usage *dto.Usage
			var relayErr *types.NewAPIError
			go func() {
				defer close(done)
				usage, relayErr = ClaudeStreamHandler(c, &http.Response{StatusCode: 200, Header: make(http.Header), Body: reader}, info)
			}()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				cancel()
				<-done
				t.Fatal("stream did not settle at its terminal event")
			}
			<-writeDone
			require.Equal(t, tc.reason, info.StreamStatus.EndReason)
			if tc.reason == relaycommon.StreamEndReasonHandlerStop {
				require.NotNil(t, relayErr)
				assert.True(t, info.StreamStatus.HasErrors())
			}
			if tc.reason == relaycommon.StreamEndReasonDone {
				require.Nil(t, relayErr)
				require.NotNil(t, usage)
				assert.Equal(t, 10, usage.PromptTokens)
				assert.Equal(t, 3, usage.CompletionTokens)
				assert.Equal(t, 1, strings.Count(w.Body.String(), "event: message_stop"))
				assert.Nil(t, info.StreamStatus.EndError)
				assert.False(t, info.StreamStatus.HasErrors())
			}
			// 校验消费日志真实使用的状态序列化，不能只检查内部结束原因。
			other := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 0, 1)
			expectedStatus := "error"
			if tc.reason == relaycommon.StreamEndReasonDone {
				expectedStatus = "ok"
			}
			assert.Equal(t, expectedStatus, other["stream_status"].(map[string]interface{})["status"])
		})
	}
}

// 实际 TCP：客户端读到协议终态后立刻关闭响应，上游保持打开。
func TestClaudeStreamCompletionOverHTTP(t *testing.T) {
	_ = newClaudeStreamTestContext(t)
	results := make(chan *relaycommon.StreamStatus, 1)
	upstreamClosed := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, completionPrefix+completionDelta+completionStop)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamClosed)
	}))
	defer upstream.Close()
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream.URL, nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		info := newClaudeStreamRelayInfo()
		info.IsStream = true
		_, _ = ClaudeStreamHandler(c, resp, info)
		results <- info.StreamStatus
	}))
	defer gateway.Close()
	resp, err := http.Get(gateway.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	var received strings.Builder
	buf := make([]byte, 1)
	for !strings.Contains(received.String(), "data: {\"type\":\"message_stop\"}\n\n") {
		n, err := resp.Body.Read(buf)
		received.Write(buf[:n])
		require.NoError(t, err)
	}
	require.NoError(t, resp.Body.Close())
	select {
	case status := <-results:
		assert.Equal(t, relaycommon.StreamEndReasonDone, status.EndReason)
		assert.False(t, status.HasErrors())
	case <-time.After(2 * time.Second):
		t.Fatal("gateway did not settle")
	}
	select {
	case <-upstreamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream connection leaked")
	}
}

func TestClaudeStreamCompletionRejectsEmptyMessageStop(t *testing.T) {
	c, w := newClaudeStreamTestContextWithRecorder(t)
	usage, err := ClaudeStreamHandler(c, newClaudeStreamResponse(completionStop), newClaudeStreamRelayInfo())
	require.Nil(t, usage)
	require.NotNil(t, err)
	assert.Equal(t, types.ErrorCodeEmptyResponse, err.GetErrorCode())
	assert.Empty(t, w.Body.String())
}
