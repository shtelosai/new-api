package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sub2apiFixture(t *testing.T, transport http.RoundTripper) (*AsyncManager, *Sub2API, *AsyncRequest) {
	t.Helper()
	p := &Sub2API{BaseURL: "https://subscription.example", Key: "test-only", Client: &http.Client{Transport: transport}, MaxPerHour: 1}
	routes := map[string]Route{"sub": {AsyncProvider: p, Token: "internal-test", Models: map[string]bool{"gpt-image-2.5-sunburst": true}}}
	m, err := openAsyncManager(AsyncConfig{DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 10, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		t.Error("local result must not use network download")
		return nil, fmt.Errorf("unexpected download")
	})}, time.Millisecond)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, m.Close()) })
	return m, p, &AsyncRequest{JobID: "job", Model: "gpt-image-2.5-sunburst", Prompt: "original prompt", Resolution: "1k", Size: "16:9"}
}

func TestSub2APIAsyncLocalResultAndHourlyLimit(t *testing.T) {
	picture := asyncPicture(t, 1672, 941)
	calls := 0
	m, _, r := sub2apiFixture(t, testTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		assert.Equal(t, "/v1/images/generations", req.URL.Path)
		assert.Equal(t, "Bearer test-only", req.Header.Get("Authorization"))
		var payload map[string]any
		require.NoError(t, common.DecodeJson(req.Body, &payload))
		assert.Equal(t, "original prompt\n画幅比例 16:9 构图 / aspect ratio 16:9", payload["prompt"])
		assert.Equal(t, float64(1), payload["n"])
		body, _ := common.Marshal(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(picture)}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}))
	_, e := m.create("sub", r)
	require.Nil(t, e)
	_, e = m.create("sub", r)
	require.Nil(t, e)
	other := *r
	other.JobID = "second"
	_, e = m.create("sub", &other)
	require.NotNil(t, e)
	assert.Equal(t, "sub2api_hourly_limit", e.Code)
	assert.True(t, e.Retryable)
	require.NoError(t, m.runOnce(context.Background()))
	m.subCalls.Wait()
	require.NoError(t, m.runOnce(context.Background()))
	require.NoError(t, m.runOnce(context.Background()))
	task, err := m.get("sub", r.JobID)
	require.NoError(t, err)
	assert.Equal(t, "succeeded", task.Status)
	assert.Equal(t, "1672x941", task.ActualSize)
	assert.Equal(t, 1, calls)
	s := &Server{Async: m, Routes: m.routes}
	req := httptest.NewRequest("GET", "/sub/v1/image-tasks/job/result", nil)
	req.Header.Set("Authorization", "Bearer internal-test")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, picture, w.Body.Bytes())
	require.NoError(t, m.Close())
	resumed, err := openAsyncManager(m.config, m.routes, m.client, time.Millisecond)
	require.NoError(t, err)
	defer resumed.Close()
	_, e = resumed.create("sub", &other)
	require.NotNil(t, e)
	assert.Equal(t, "sub2api_hourly_limit", e.Code)
	require.NoError(t, resumed.runOnce(context.Background()))
	assert.Equal(t, 1, calls)
}

func TestSub2APIUnsupportedOptions(t *testing.T) {
	cases := map[string]func(*AsyncRequest){
		"2k": func(r *AsyncRequest) { r.Resolution = "2k" }, "4k": func(r *AsyncRequest) { r.Resolution = "4k" },
		"reference": func(r *AsyncRequest) { r.ImageURLs = []string{"https://images.example/a.png"} },
		"mask":      func(r *AsyncRequest) { r.MaskURL = "https://images.example/m.png" },
		"quality":   func(r *AsyncRequest) { r.Quality = "low" }, "webp": func(r *AsyncRequest) { r.OutputFormat = "webp" },
		"compression": func(r *AsyncRequest) { r.OutputCompression = common.GetPointer(0) },
		"transparent": func(r *AsyncRequest) { r.Background = "transparent" }, "ratio": func(r *AsyncRequest) { r.Size = "21:9" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := &AsyncRequest{JobID: "job", Model: "gpt-image-2.5-sunburst", Prompt: "test", Resolution: "1k", Size: "1:1"}
			change(r)
			e := (&Sub2API{}).ValidateTask(r)
			require.NotNil(t, e)
			assert.Equal(t, "unsupported_image_options", e.Code)
			assert.True(t, e.Retryable)
			assert.False(t, e.Unknown)
		})
	}
}

func TestSub2APISubmissionFailureAndRestartNeverReplay(t *testing.T) {
	for _, status := range []int{400, 401, 402, 403, 429, 500, 0, 200} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			m, _, r := sub2apiFixture(t, testTransport(func(*http.Request) (*http.Response, error) {
				calls++
				if status == 0 {
					return nil, context.DeadlineExceeded
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(bytes.NewBufferString("{}"))}, nil
			}))
			_, e := m.create("sub", r)
			require.Nil(t, e)
			require.NoError(t, m.runOnce(context.Background()))
			m.subCalls.Wait()
			task, err := m.get("sub", r.JobID)
			require.NoError(t, err)
			assert.Equal(t, "failed", task.Status)
			assert.True(t, task.Retryable)
			assert.Equal(t, "sub2api_submit_rejected", task.ErrorCode)
			assert.Empty(t, task.ProviderTaskID)
			require.NoError(t, m.Close())
			resumed, err := openAsyncManager(m.config, m.routes, m.client, time.Millisecond)
			require.NoError(t, err)
			defer resumed.Close()
			require.NoError(t, resumed.runOnce(context.Background()))
			assert.Equal(t, 1, calls)
		})
	}
	t.Run("interrupted submitting", func(t *testing.T) {
		m, _, r := sub2apiFixture(t, testTransport(func(*http.Request) (*http.Response, error) { t.Error("must not replay"); return nil, context.Canceled }))
		task, e := m.create("sub", r)
		require.Nil(t, e)
		task.Status = "submitting"
		require.NoError(t, m.save(task))
		require.NoError(t, m.Close())
		resumed, err := openAsyncManager(m.config, m.routes, m.client, time.Millisecond)
		require.NoError(t, err)
		defer resumed.Close()
		require.NoError(t, resumed.runOnce(context.Background()))
		task, err = resumed.get("sub", r.JobID)
		require.NoError(t, err)
		assert.Equal(t, "unknown", task.Status)
		assert.False(t, task.Retryable)
	})
}

func TestSub2APIRatioMismatch(t *testing.T) {
	picture := asyncPicture(t, 1254, 1254)
	m, _, r := sub2apiFixture(t, testTransport(func(*http.Request) (*http.Response, error) {
		body, _ := common.Marshal(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(picture)}}})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}))
	_, e := m.create("sub", r)
	require.Nil(t, e)
	require.NoError(t, m.runOnce(context.Background()))
	m.subCalls.Wait()
	require.NoError(t, m.runOnce(context.Background()))
	require.NoError(t, m.runOnce(context.Background()))
	task, err := m.get("sub", r.JobID)
	require.NoError(t, err)
	assert.Equal(t, "failed", task.Status)
	assert.Equal(t, "sub2api_ratio_mismatch", task.ErrorCode)
	assert.True(t, task.Retryable)
	assert.Empty(t, task.ProviderTaskID)
}

func TestSub2APIInFlightDoesNotBlockOtherProvidersAndCloseCancels(t *testing.T) {
	started := make(chan struct{})
	m, p, r := sub2apiFixture(t, testTransport(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	}))
	p.MaxConcurrent = 1
	p.MaxPerHour = 10
	polls := 0
	m.routes["kie"] = Route{Models: map[string]bool{r.Model: true}, AsyncProvider: &Kie{BaseURL: "https://kie.example", Client: &http.Client{Transport: testTransport(func(req *http.Request) (*http.Response, error) {
		body := `{"code":200,"data":{"taskId":"kie-task"}}`
		if req.Method == "GET" {
			polls++
			body = `{"code":200,"data":{"taskId":"kie-task","state":"generating"}}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
	})}}}
	_, problem := m.create("sub", r)
	require.Nil(t, problem)
	require.NoError(t, m.runOnce(context.Background()))
	<-started
	queued := *r
	queued.JobID = "queued-sub"
	_, problem = m.create("sub", &queued)
	require.Nil(t, problem)
	_, problem = m.create("kie", r)
	require.Nil(t, problem)
	require.NoError(t, m.runOnce(context.Background()))
	require.NoError(t, m.runOnce(context.Background()))
	assert.Equal(t, 1, polls)
	active, err := m.get("sub", r.JobID)
	require.NoError(t, err)
	assert.Equal(t, "submitting", active.Status)
	pending, err := m.get("sub", queued.JobID)
	require.NoError(t, err)
	assert.Equal(t, "queued", pending.Status)
	ordinary, err := m.get("kie", r.JobID)
	require.NoError(t, err)
	assert.Equal(t, "running", ordinary.Status)
	require.NoError(t, m.Close())
	resumed, err := openAsyncManager(m.config, m.routes, m.client, time.Millisecond)
	require.NoError(t, err)
	defer resumed.Close()
	stopped, err := resumed.get("sub", r.JobID)
	require.NoError(t, err)
	assert.Equal(t, "failed", stopped.Status)
	assert.True(t, stopped.Retryable)
	assert.Empty(t, stopped.ProviderTaskID)
}

func TestSub2APIInvalidResultsRemainUnaccepted(t *testing.T) {
	for _, body := range []string{`invalid`, `{"data":[]}`, `{"data":[{"b64_json":"!!!"}]}`, `{"data":[{"b64_json":"bm90IGFuIGltYWdl"}]}`} {
		t.Run(body, func(t *testing.T) {
			m, _, r := sub2apiFixture(t, testTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewBufferString(body))}, nil
			}))
			_, problem := m.create("sub", r)
			require.Nil(t, problem)
			require.NoError(t, m.runOnce(context.Background()))
			m.subCalls.Wait()
			task, err := m.get("sub", r.JobID)
			require.NoError(t, err)
			assert.Equal(t, "failed", task.Status)
			assert.True(t, task.Retryable)
			assert.Equal(t, "sub2api_submit_rejected", task.ErrorCode)
			assert.Empty(t, task.ProviderTaskID)
		})
	}
}
