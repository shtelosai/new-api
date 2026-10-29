package openai

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 阻塞上游读取与可控下游写入，用通道固定收尾/取消竞态，无需等待网络时序。
type responsesLifecycleBody struct {
	data      *strings.Reader
	waiting   chan struct{}
	closed    chan struct{}
	readOnce  sync.Once
	closeOnce sync.Once
	readErr   error
	closeErr  error
}

func (b *responsesLifecycleBody) Read(p []byte) (int, error) {
	if b.data.Len() > 0 {
		return b.data.Read(p)
	}
	b.readOnce.Do(func() { close(b.waiting) })
	if b.readErr != nil {
		return 0, b.readErr
	}
	<-b.closed
	return 0, b.closeErr
}

func (b *responsesLifecycleBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

type responsesLifecycleWriter struct {
	*httptest.ResponseRecorder
	body              *responsesLifecycleBody
	cancel            context.CancelFunc
	cancelBeforeFlush bool
	waitForCleanup    bool
	failWrite         int
	writeErr          error
	flushErr          error
	writes            int
	needle            string
}

func (w *responsesLifecycleWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failWrite {
		return 0, w.writeErr
	}
	n, err := w.ResponseRecorder.Write(p)
	if w.cancelBeforeFlush && strings.Contains(w.Body.String(), w.needle) {
		<-w.body.waiting
		w.cancel()
		<-w.body.closed
	}
	return n, err
}

func (w *responsesLifecycleWriter) Flush() {
	w.ResponseRecorder.Flush()
	if !strings.Contains(w.Body.String(), w.needle) || w.body == nil {
		return
	}
	<-w.body.waiting
	if w.cancel != nil {
		w.cancel()
	}
	if w.waitForCleanup {
		<-w.body.closed
	}
}

func (w *responsesLifecycleWriter) FlushError() error {
	return w.flushErr
}

func TestOaiResponsesStreamHandlerCompletionLifecycle(t *testing.T) {
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 1
	t.Cleanup(func() {
		gin.SetMode(oldMode)
		constant.StreamingTimeout = oldTimeout
	})

	for _, tc := range []struct {
		name              string
		event             string
		responseStatus    string
		cancel            bool
		cancelBeforeFlush bool
		closeEOF          bool
		eof               bool
		scannerError      bool
		priorError        bool
		failWrite         int
		failFlush         bool
		wantEnd           relaycommon.StreamEndReason
		wantError         bool
	}{
		{name: "completed without upstream EOF", event: "response.completed", responseStatus: "completed", wantEnd: relaycommon.StreamEndReasonDone},
		{name: "done alias without upstream EOF", event: "response.done", responseStatus: "completed", wantEnd: relaycommon.StreamEndReasonDone},
		{name: "disconnect during terminal flush", event: "response.completed", responseStatus: "completed", cancel: true, wantEnd: relaycommon.StreamEndReasonDone},
		{name: "completion then cancellation and cleanup EOF", event: "response.completed", responseStatus: "completed", cancel: true, closeEOF: true, wantEnd: relaycommon.StreamEndReasonDone},
		{name: "midstream disconnect", event: "response.output_text.delta", cancel: true, wantEnd: relaycommon.StreamEndReasonClientGone, wantError: true},
		{name: "midstream disconnect followed by cleanup EOF", event: "response.output_text.delta", cancel: true, closeEOF: true, wantEnd: relaycommon.StreamEndReasonClientGone, wantError: true},
		{name: "cancellation before flush", event: "response.completed", responseStatus: "completed", cancel: true, cancelBeforeFlush: true, wantEnd: relaycommon.StreamEndReasonClientGone, wantError: true},
		{name: "event write failure", event: "response.completed", responseStatus: "completed", failWrite: 1, wantEnd: relaycommon.StreamEndReasonHandlerStop, wantError: true},
		{name: "data write failure", event: "response.completed", responseStatus: "completed", failWrite: 2, wantEnd: relaycommon.StreamEndReasonHandlerStop, wantError: true},
		{name: "delimiter write failure", event: "response.completed", responseStatus: "completed", failWrite: 3, wantEnd: relaycommon.StreamEndReasonHandlerStop, wantError: true},
		{name: "flush failure", event: "response.completed", responseStatus: "completed", failFlush: true, wantEnd: relaycommon.StreamEndReasonHandlerStop, wantError: true},
		{name: "protocol error before completion", event: "response.completed", responseStatus: "completed", priorError: true, cancel: true, wantEnd: relaycommon.StreamEndReasonClientGone, wantError: true},
		{name: "upstream read failure before handler returns", event: "response.completed", responseStatus: "completed", scannerError: true, wantEnd: relaycommon.StreamEndReasonScannerErr, wantError: true},
		{name: "done with failed response", event: "response.done", responseStatus: "failed", eof: true, wantEnd: relaycommon.StreamEndReasonEOF, wantError: true},
		{name: "done with incomplete response", event: "response.done", responseStatus: "incomplete", eof: true, wantEnd: relaycommon.StreamEndReasonEOF},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := fmt.Sprintf(`{"type":%q,"response":{"status":%q,"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12},"output":[{"id":"img_1","type":"image_generation_call","status":"completed","result":"image-data"}]}}`, tc.event, tc.responseStatus)
			if tc.responseStatus == "" {
				payload = `{"type":"response.output_text.delta","delta":"hello"}`
			}
			sse := "data: " + payload + "\n\n"
			if tc.priorError {
				sse = "data: invalid JSON\n\n" + sse
			}
			body := &responsesLifecycleBody{data: strings.NewReader(sse), waiting: make(chan struct{}), closed: make(chan struct{}), closeErr: io.ErrClosedPipe}
			if tc.closeEOF {
				body.closeErr = io.EOF
			}
			if tc.scannerError {
				body.readErr = io.ErrUnexpectedEOF
			}
			t.Cleanup(func() { require.NoError(t, body.Close()) })
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			writer := &responsesLifecycleWriter{
				ResponseRecorder: httptest.NewRecorder(), body: body, needle: payload + "\n\n",
				cancelBeforeFlush: tc.cancelBeforeFlush, waitForCleanup: tc.cancel || tc.scannerError,
				failWrite: tc.failWrite, writeErr: errors.New("downstream write failed"),
			}
			if tc.cancel {
				writer.cancel = cancel
			}
			if tc.failFlush {
				writer.flushErr = writer.writeErr
			}
			var upstream io.ReadCloser = body
			if tc.eof {
				upstream = io.NopCloser(strings.NewReader(sse))
				writer.body = nil
			}
			c, _ := gin.CreateTestContext(writer)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
			info := &relaycommon.RelayInfo{
				OriginModelName: "gpt-4o", IsStream: true, DisablePing: true, StartTime: time.Now(),
				RelayFormat: types.RelayFormatOpenAIResponses,
				ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o"},
			}
			info.SetEstimatePromptTokens(7)
			resp := &http.Response{StatusCode: http.StatusOK, Body: upstream, Header: http.Header{"Content-Type": {"text/event-stream"}}}
			usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			require.NotNil(t, info.StreamStatus)
			assert.Equal(t, tc.wantEnd, info.StreamStatus.EndReason)
			log := service.GenerateTextOtherInfo(c, info, 1, 1, 1, 0, 1, 0, 1)["stream_status"].(map[string]any)
			assert.Equal(t, tc.wantError, log["status"] == "error")
			if tc.responseStatus != "" {
				assert.Equal(t, 7, usage.PromptTokens)
				assert.Equal(t, 5, usage.CompletionTokens)
				assert.Equal(t, 12, usage.TotalTokens)
			}
			if tc.failWrite == 0 {
				assert.Contains(t, writer.Body.String(), payload+"\n\n")
			} else {
				assert.ErrorIs(t, info.StreamStatus.EndError, writer.writeErr)
			}
			if tc.failFlush {
				assert.ErrorIs(t, info.StreamStatus.EndError, writer.flushErr)
			}
			if !tc.eof {
				select {
				case <-body.closed:
				default:
					t.Fatal("upstream body was not closed")
				}
			}
		})
	}
}

// 使用真实 TCP 连接验证：终态送达后主动关闭上游，不等待上游 EOF。
func TestResponsesCompletionOverHTTPClosesOpenUpstream(t *testing.T) {
	oldTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 10
	t.Cleanup(func() { constant.StreamingTimeout = oldTimeout })
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":7,\"output_tokens\":5,\"total_tokens\":12}}}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()
	type outcome struct {
		reason relaycommon.StreamEndReason
		failed bool
		tokens int
		err    *types.NewAPIError
	}
	finished := make(chan outcome, 1)
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream.URL, nil)
		if err != nil {
			t.Error(err)
			return
		}
		resp, err := upstream.Client().Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		info := &relaycommon.RelayInfo{OriginModelName: "gpt-4o", IsStream: true, DisablePing: true, StartTime: time.Now(), RelayFormat: types.RelayFormatOpenAIResponses, ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o"}}
		usage, apiErr := OaiResponsesStreamHandler(c, info, resp)
		finished <- outcome{info.StreamStatus.EndReason, info.StreamStatus.HasErrors(), usage.TotalTokens, apiErr}
	}))
	defer gateway.Close()
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(gateway.URL)
	require.NoError(t, err)
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	var event strings.Builder
	for {
		line, err := reader.ReadString('\n')
		require.NoError(t, err)
		event.WriteString(line)
		if line == "\n" {
			break
		}
	}
	require.Contains(t, event.String(), "response.completed")
	require.NoError(t, resp.Body.Close())
	select {
	case result := <-finished:
		require.Nil(t, result.err)
		assert.Equal(t, relaycommon.StreamEndReasonDone, result.reason)
		assert.False(t, result.failed)
		assert.Equal(t, 12, result.tokens)
	case <-time.After(3 * time.Second):
		t.Fatal("网关等待未关闭的上游连接，未及时收尾")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(3 * time.Second):
		t.Fatal("上游请求未取消")
	}
}
