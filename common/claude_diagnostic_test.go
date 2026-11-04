package common

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeDiagnosticStablePrefixAndBreakpoint(t *testing.T) {
	s := &claudeDiagnosticSink{key: []byte("test-only-key")}
	a := []byte(`{"model":"claude-sonnet-5-5","system":[{"type":"text","text":"secret-system"}],"messages":[{"role":"user","content":[{"type":"text","text":"secret-input","cache_control":{"type":"ephemeral"}}]}]}`)
	b := []byte(`{"model":"claude-sonnet-5-5","system":[{"type":"text","text":"secret-system"}],"messages":[{"role":"user","content":[{"type":"text","text":"secret-input"}]},{"role":"assistant","content":[{"type":"text","text":"answer"}]},{"role":"user","content":[{"type":"text","text":"next","cache_control":{"type":"ephemeral"}}]}]}`)
	x, y := s.summarize(a), s.summarize(b)
	xb, yb := x["blocks"].([]map[string]any), y["blocks"].([]map[string]any)
	require.Greater(t, len(yb), len(xb))
	assert.Equal(t, xb, yb[:len(xb)])
	assert.NotEqual(t, x["checkpoints"], y["checkpoints"])
	z := s.summarize(bytes.ReplaceAll(b, []byte("secret-system"), []byte("changed")))
	assert.NotEqual(t, yb[0]["prefix_hash"], z["blocks"].([]map[string]any)[0]["prefix_hash"])
	raw, err := Marshal(y)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "secret-input")
	assert.NotContains(t, string(raw), "secret-system")
}
func TestClaudeDiagnosticPreservesWireAndRawUsage(t *testing.T) {
	old := diagnosticSink
	defer func() { diagnosticSink = old }()
	diagnosticOnce.Do(func() {})
	diagnosticSink = &claudeDiagnosticSink{key: []byte("test-key"), until: time.Now().Add(time.Hour), queue: make(chan map[string]any, 10)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	c.Request.Header.Set("X-Twork-Client-Version", "4.3.3")
	c.Set("token_name", "twork_85")
	c.Set(RequestIdKey, "request-test")
	c.Set("conversation_hash", "session-hash")
	body := `{"model":"claude-opus-5-5","messages":[],"metadata":{"user_id":"private-session"}}`
	CaptureClaudeDiagnosticInbound(c, []byte(body))
	req := httptest.NewRequest("POST", "https://provider.test/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer DO_NOT_STORE")
	observe := ObserveClaudeDiagnosticAttempt(c, req, 269, "claude-opus-5-5")
	actual, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	assert.Equal(t, body, string(actual))
	require.NoError(t, req.Body.Close())
	wire := "event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[{\"text\":\"private-output\"}],\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":90000}}}\r\n\r\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":23}}\n\n"
	resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(wire))}
	observe(resp, nil)
	actual, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, wire, string(actual))
	require.NoError(t, resp.Body.Close())
	require.Len(t, diagnosticSink.queue, 3)
	var events []map[string]any
	for len(diagnosticSink.queue) > 0 {
		events = append(events, <-diagnosticSink.queue)
	}
	assert.Equal(t, "4.3.3", events[0]["headers"].(map[string]string)["X-Twork-Client-Version"])
	usage := events[2]["raw_usage_events"].([]any)
	require.Len(t, usage, 2)
	assert.Equal(t, float64(0), usage[0].(map[string]any)["message_usage"].(map[string]any)["cache_read_input_tokens"])
	assert.Equal(t, true, events[2]["read_to_eof"])
	encoded, err := Marshal(events)
	require.NoError(t, err)
	for _, secret := range []string{"DO_NOT_STORE", "private-session", "private-output"} {
		assert.NotContains(t, string(encoded), secret)
	}
	assert.Empty(t, diagnosticSlots)
}
func TestClaudeDiagnosticPartialAndUsageMissing(t *testing.T) {
	var got []byte
	complete := true
	calls := 0
	b := &diagnosticBody{ReadCloser: io.NopCloser(strings.NewReader("abcdef")), done: func(p []byte, _ bool, eof bool) { got = p; complete = eof; calls++ }}
	p := make([]byte, 2)
	_, err := b.Read(p)
	require.NoError(t, err)
	require.NoError(t, b.Close())
	require.NoError(t, b.Close())
	assert.Equal(t, []byte("ab"), got)
	assert.False(t, complete)
	assert.Equal(t, 1, calls)
	usage := diagnosticUsage([]byte(`{"usage":{"input_tokens":1}}`), false)
	require.Len(t, usage, 1)
	_, exists := usage[0].(map[string]any)["usage"].(map[string]any)["cache_read_input_tokens"]
	assert.False(t, exists)
}

func TestClaudeDiagnosticExpiredAndNonClaudeAreNoops(t *testing.T) {
	old := diagnosticSink
	defer func() { diagnosticSink = old }()
	diagnosticOnce.Do(func() {})
	diagnosticSink = &claudeDiagnosticSink{key: []byte("test"), until: time.Now().Add(-time.Second), queue: make(chan map[string]any, 4)}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
	req := httptest.NewRequest("POST", "https://upstream.test", strings.NewReader("unchanged"))
	original := req.Body
	ObserveClaudeDiagnosticAttempt(c, req, 269, "claude-opus-5-5")(nil, nil)
	assert.Same(t, original, req.Body)
	assert.Empty(t, diagnosticSink.queue)
	diagnosticSink.until = time.Now().Add(time.Hour)
	CaptureClaudeDiagnosticInbound(c, []byte(`{"model":"gpt-6.1-sol"}`))
	ObserveClaudeDiagnosticAttempt(c, req, 266, "gpt-6.1-sol")(nil, nil)
	assert.Same(t, original, req.Body)
	assert.Empty(t, diagnosticSink.queue)
}
func TestClaudeDiagnosticQueueOverflowDoesNotBlock(t *testing.T) {
	s := &claudeDiagnosticSink{queue: make(chan map[string]any, 1)}
	s.emit(map[string]any{"test": 1})
	s.emit(map[string]any{"test": 2})
	assert.Equal(t, uint64(1), s.dropped.Load())
}
