package common

// 临时诊断只保存 HMAC 指纹、结构和上游 usage，不保存正文、凭据或原始会话 ID。
import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const diagnosticLimit = 32 << 20

var diagnosticSlots = make(chan struct{}, 16)
var diagnosticOnce sync.Once
var diagnosticSink *claudeDiagnosticSink

type claudeDiagnosticSink struct {
	path    string
	key     []byte
	until   time.Time
	queue   chan map[string]any
	dropped atomic.Uint64
}

func claudeDiagnostics() *claudeDiagnosticSink {
	diagnosticOnce.Do(func() {
		dir := os.Getenv("CLAUDE_DIAGNOSTIC_DIR")
		until, err := time.Parse(time.RFC3339, os.Getenv("CLAUDE_DIAGNOSTIC_UNTIL"))
		if dir == "" || err != nil || !time.Now().Before(until) {
			return
		}
		key, err := os.ReadFile(filepath.Join(dir, "hmac.key"))
		if err != nil || len(key) < 32 {
			return
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return
		}
		diagnosticSink = &claudeDiagnosticSink{path: filepath.Join(dir, "events.jsonl"), key: key, until: until, queue: make(chan map[string]any, 32)}
		go diagnosticSink.run()
		go diagnosticSink.status()
	})
	if diagnosticSink == nil || !time.Now().Before(diagnosticSink.until) {
		return nil
	}
	return diagnosticSink
}
func (s *claudeDiagnosticSink) status() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		b, _ := Marshal(map[string]any{"at": time.Now().UTC().Format(time.RFC3339Nano), "until": s.until, "active": time.Now().Before(s.until), "dropped": s.dropped.Load(), "queue": len(s.queue), "active_bodies": len(diagnosticSlots)})
		p := filepath.Join(filepath.Dir(s.path), "status.json")
		if os.WriteFile(p+".tmp", b, 0600) == nil {
			_ = os.Rename(p+".tmp", p)
		}
		if !time.Now().Before(s.until) {
			return
		}
	}
}
func (s *claudeDiagnosticSink) run() {
	for event := range s.queue {
		data, err := Marshal(event)
		if err != nil {
			s.dropped.Add(1)
			continue
		}
		fi, err := os.Stat(s.path)
		if err == nil && fi.Size()+int64(len(data)) > 4<<30 {
			s.dropped.Add(1)
			continue
		}
		f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			s.dropped.Add(1)
			continue
		}
		_, err = f.Write(append(data, '\n'))
		_ = f.Close()
		if err != nil {
			s.dropped.Add(1)
		}
	}
}
func (s *claudeDiagnosticSink) emit(e map[string]any) {
	e["at"] = time.Now().UTC().Format(time.RFC3339Nano)
	e["dropped_before"] = s.dropped.Load()
	select {
	case s.queue <- e:
	default:
		s.dropped.Add(1)
	}
}
func (s *claudeDiagnosticSink) hash(b []byte) string {
	h := hmac.New(sha256.New, s.key)
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}
func (s *claudeDiagnosticSink) valueHash(v any) string { b, _ := Marshal(v); return s.hash(b) }
func diagHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range []string{"X-Twork-Client-Version", "X-Twork-Omp-Version", "X-Twork-Agent-Runtime", "X-Twork-Wire-Api", "User-Agent", "Anthropic-Version", "Anthropic-Beta"} {
		if v := h.Get(k); v != "" && len(v) <= 1024 {
			out[k] = v
		}
	}
	return out
}
func diagStrip(v any) any {
	switch x := v.(type) {
	case map[string]any:
		o := map[string]any{}
		for k, v := range x {
			if k != "cache_control" {
				o[k] = diagStrip(v)
			}
		}
		return o
	case []any:
		o := make([]any, len(x))
		for i, v := range x {
			o[i] = diagStrip(v)
		}
		return o
	default:
		return v
	}
}
func (s *claudeDiagnosticSink) summarize(b []byte) map[string]any {
	out := map[string]any{"bytes": len(b), "wire_hash": s.hash(b)}
	var body map[string]any
	if err := Unmarshal(b, &body); err != nil {
		out["parse_error"] = true
		return out
	}
	fields := map[string]string{}
	for k, v := range body {
		if k != "messages" {
			fields[k] = s.valueHash(diagStrip(v))
		}
	}
	out["fields"] = fields
	var blocks []map[string]any
	var points []map[string]any
	chain := ""
	add := func(path string, v any, role string) {
		stripped := diagStrip(v)
		h := s.valueHash(stripped)
		chain = s.hash([]byte(chain + "|" + role + "|" + h))
		item := map[string]any{"path": path, "role": role, "hash": h, "prefix_hash": chain}
		if m, ok := v.(map[string]any); ok {
			item["type"] = m["type"]
			fh := map[string]string{}
			for k, v := range m {
				if k != "cache_control" {
					fh[k] = s.valueHash(v)
				}
			}
			item["field_hashes"] = fh
			if cc, ok := m["cache_control"]; ok {
				points = append(points, map[string]any{"path": path, "control": cc, "prefix_hash": chain})
			}
		}
		blocks = append(blocks, item)
	}
	// 按 Anthropic 缓存顺序 tools → system → messages 计算累积指纹。
	for _, field := range []string{"tools", "system"} {
		switch v := body[field].(type) {
		case []any:
			for i, x := range v {
				add(field+"."+diagIndex(i), x, field)
			}
		case string:
			add(field, v, field)
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		for i, v := range messages {
			if len(blocks) > 20000 {
				out["structure_truncated"] = true
				break
			}
			m, ok := v.(map[string]any)
			if !ok {
				continue
			}
			role, _ := m["role"].(string)
			path := "messages." + diagIndex(i)
			// 消息字段、string/array 表示差异同样保留，不能把规范化当成字节一致。
			extra := map[string]any{}
			for k, v := range m {
				if k != "content" {
					extra[k] = v
				}
			}
			add(path+".envelope", extra, role)
			switch c := m["content"].(type) {
			case []any:
				for j, x := range c {
					add(path+".content."+diagIndex(j), x, role)
				}
			default:
				add(path+".content", c, role)
			}
		}
	}
	out["blocks"] = blocks
	out["checkpoints"] = points
	out["semantic_hash"] = s.valueHash(diagStrip(body))
	return out
}
func diagIndex(i int) string { b, _ := Marshal(i); return string(b) }

// 在分发器读取到原始 JSON、尚未修改前记录。重复解析只记一次。
func CaptureClaudeDiagnosticInbound(c *gin.Context, body []byte) {
	s := claudeDiagnostics()
	if s == nil || c.GetBool("claude_diag_inbound") || !strings.HasPrefix(gjson.GetBytes(body, "model").String(), "claude-") {
		return
	}
	c.Set("claude_diag_inbound", true)
	select {
	case diagnosticSlots <- struct{}{}:
		defer func() { <-diagnosticSlots }()
	default:
		s.dropped.Add(1)
		s.emit(map[string]any{"phase": "inbound_skipped_busy", "request_id": GetContextKeyString(c, RequestIdKey)})
		return
	}
	e := map[string]any{"phase": "inbound", "request_id": GetContextKeyString(c, RequestIdKey), "token_name": c.GetString("token_name"), "headers": diagHeaders(c.Request.Header), "model": gjson.GetBytes(body, "model").String()}
	e["conversation_hash"] = c.GetString("conversation_hash")
	if len(body) > diagnosticLimit {
		e["oversize"] = true
		e["bytes"] = len(body)
	} else {
		e["body"] = s.summarize(body)
	}
	s.emit(e)
}

type diagnosticBody struct {
	io.ReadCloser
	mu       sync.Mutex
	data     []byte
	overflow bool
	complete bool
	once     sync.Once
	done     func([]byte, bool, bool)
}

func (b *diagnosticBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	if err == io.EOF {
		b.complete = true
	}
	if !b.overflow {
		if len(b.data)+n <= diagnosticLimit {
			b.data = append(b.data, p[:n]...)
		} else {
			b.overflow = true
			b.data = nil
		}
	}
	b.mu.Unlock()
	if err != nil {
		b.finish()
	}
	return n, err
}
func (b *diagnosticBody) Close() error { err := b.ReadCloser.Close(); b.finish(); return err }
func (b *diagnosticBody) finish() {
	b.once.Do(func() {
		b.mu.Lock()
		data := b.data
		overflow := b.overflow
		complete := b.complete
		b.data = nil
		b.mu.Unlock()
		b.done(data, overflow, complete)
	})
}

// 包装真实 HTTP 字节流，不重序列化正文，不更改 Content-Length、重试或 SSE。
func ObserveClaudeDiagnosticAttempt(c *gin.Context, req *http.Request, channel int, model string) func(*http.Response, error) {
	s := claudeDiagnostics()
	if s == nil || (!c.GetBool("claude_diag_inbound") && !strings.HasPrefix(model, "claude-")) {
		return func(*http.Response, error) {}
	}
	attempt := c.GetInt("claude_diag_attempt") + 1
	c.Set("claude_diag_attempt", attempt)
	id := GetContextKeyString(c, RequestIdKey)
	user := c.GetString("token_name")
	conv := c.GetString("conversation_hash")
	headers := diagHeaders(c.Request.Header)
	base := func(phase string) map[string]any {
		return map[string]any{"phase": phase, "request_id": id, "token_name": user, "conversation_hash": conv, "attempt": attempt, "channel": channel, "upstream_model": model, "client_headers": headers}
	}
	select {
	case diagnosticSlots <- struct{}{}:
	default:
		e := base("skipped_busy")
		s.dropped.Add(1)
		s.emit(e)
		return func(*http.Response, error) {}
	}
	var release sync.Once
	free := func() { release.Do(func() { <-diagnosticSlots }) }
	if req.Body != nil {
		req.Body = &diagnosticBody{ReadCloser: req.Body, done: func(b []byte, overflow bool, complete bool) {
			e := base("outbound")
			e["headers"] = diagHeaders(req.Header)
			e["credential_hash"] = s.hash([]byte(req.Header.Get("Authorization") + "|" + req.Header.Get("X-Api-Key")))
			e["host"] = req.URL.Host
			e["oversize"] = overflow
			e["read_to_eof"] = complete
			if !overflow {
				e["body"] = s.summarize(b)
			}
			s.emit(e)
		}}
	}
	return func(resp *http.Response, err error) {
		e := base("upstream_response")
		if err != nil {
			free()
			e["transport_error"] = true
			s.emit(e)
			return
		}
		if resp == nil {
			free()
			return
		}
		e["status"] = resp.StatusCode
		e["headers_received_at"] = time.Now().UTC().Format(time.RFC3339Nano)
		ids := map[string]string{}
		for _, k := range []string{"Request-Id", "X-Request-Id", "X-Oneapi-Request-Id", "X-Litellm-Call-Id", "CF-Ray"} {
			if v := resp.Header.Get(k); v != "" && len(v) <= 256 {
				ids[k] = v
			}
		}
		e["upstream_ids"] = ids
		stream := strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream")
		resp.Body = &diagnosticBody{ReadCloser: resp.Body, done: func(b []byte, overflow bool, complete bool) {
			e["oversize"] = overflow
			e["read_to_eof"] = complete
			defer free()
			e["raw_usage_events"] = diagnosticUsage(b, stream)
			s.emit(e)
		}}
	}
}
func diagnosticUsage(b []byte, stream bool) []any {
	var out []any
	consume := func(data []byte) {
		var v map[string]any
		if Unmarshal(data, &v) != nil {
			return
		}
		e := map[string]any{}
		for _, key := range []string{"usage", "provider_usage"} {
			if x, ok := v[key]; ok {
				e[key] = x
			}
		}
		if m, ok := v["message"].(map[string]any); ok {
			if u, ok := m["usage"]; ok {
				e["message_usage"] = u
			}
		}
		if r, ok := v["response"].(map[string]any); ok {
			if u, ok := r["usage"]; ok {
				e["response_usage"] = u
			}
		}
		if len(e) > 0 {
			e["event_type"] = v["type"]
			if d, ok := v["delta"].(map[string]any); ok {
				e["stop_reason"] = d["stop_reason"]
			}
			if len(out) < 256 {
				out = append(out, e)
			}
		}
	}
	if !stream {
		consume(b)
		return out
	}
	// SSE 数据帧允许多条 data 行；不记录生成文本或 thinking。
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	for _, frame := range bytes.Split(b, []byte("\n\n")) {
		var lines [][]byte
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				lines = append(lines, bytes.TrimSpace(line[5:]))
			}
		}
		if len(lines) > 0 {
			consume(bytes.Join(lines, []byte("\n")))
		}
	}
	return out
}
