package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func asyncPicture(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, width, height))))
	return b.Bytes()
}

func TestKieTaskMapsFamiliesAndEditInputs(t *testing.T) {
	picture := asyncPicture(t, 1024, 1024)
	var submits, uploads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer supplier-key", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/api/file-stream-upload":
			uploads.Add(1)
			require.NoError(t, r.ParseMultipartForm(1<<20))
			defer r.MultipartForm.RemoveAll()
			f, _, err := r.FormFile("file")
			require.NoError(t, err)
			defer f.Close()
			b, err := io.ReadAll(f)
			require.NoError(t, err)
			assert.Equal(t, picture, b)
			assert.NotEmpty(t, r.FormValue("fileName"))
			io.WriteString(w, `{"success":true,"code":200,"data":{"downloadUrl":"https://files.example/input.png"}}`)
		case "/api/v1/jobs/createTask":
			submits.Add(1)
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			model := gjson.GetBytes(b, "model").String()
			assert.Contains(t, []string{"gpt-image-2-5-flare-text-to-image", "gpt-image-2-5-flare-image-to-image", "gpt-image-2-5-sunburst-text-to-image", "gpt-image-2-5-sunburst-image-to-image"}, model)
			if strings.Contains(model, "image-to-image") {
				assert.Equal(t, "https://files.example/input.png", gjson.GetBytes(b, "input.input_urls.0").String())
			}
			expected := "1K"
			if strings.Contains(model, "sunburst") {
				expected = "4K"
			}
			assert.Equal(t, expected, gjson.GetBytes(b, "input.resolution").String())
			assert.Equal(t, "1:1", gjson.GetBytes(b, "input.aspect_ratio").String())
			assert.Equal(t, "transparent", gjson.GetBytes(b, "input.background").String())
			io.WriteString(w, `{"code":200,"data":{"taskId":"kie_abc"}}`)
		case "/api/v1/jobs/recordInfo":
			assert.Equal(t, "kie_abc", r.URL.Query().Get("taskId"))
			io.WriteString(w, `{"code":200,"data":{"taskId":"kie_abc","state":"success","resultJson":"{\"resultUrls\":[\"https://files.example/output.png\"]}"}}`)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	provider := &Kie{BaseURL: upstream.URL, UploadBaseURL: upstream.URL, Key: "supplier-key", Client: upstream.Client()}
	for _, family := range []string{"flare", "sunburst"} {
		for _, edit := range []bool{false, true} {
			resolution := "1k"
			if family == "sunburst" {
				resolution = "4k"
			}
			req := &AsyncRequest{JobID: "job", Model: "gpt-image-2.5-" + family, Prompt: "杯子", Size: "1:1", Resolution: resolution, Background: "transparent"}
			if edit {
				req.ImageURLs = []string{"data:image/png;base64," + base64.StdEncoding.EncodeToString(picture)}
			}
			require.Nil(t, provider.ValidateTask(req))
			id, err := provider.SubmitTask(context.Background(), req)
			require.Nil(t, err)
			assert.Equal(t, "kie_abc", id)
			result, err := provider.PollTask(context.Background(), id)
			require.Nil(t, err)
			assert.Equal(t, []string{"https://files.example/output.png"}, result.URLs)
		}
	}
	assert.Equal(t, int32(4), submits.Load())
	assert.Equal(t, int32(2), uploads.Load())
}

func TestAsyncHTTPPersistsIdempotencyAndSurvivesRestart(t *testing.T) {
	var submits, polls atomic.Int32
	picture := asyncPicture(t, 2880, 2880)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/images/generations":
			submits.Add(1)
			b, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			assert.Equal(t, "1:1", gjson.GetBytes(b, "size").String())
			assert.Equal(t, "4k", gjson.GetBytes(b, "resolution").String())
			assert.Equal(t, int64(1), gjson.GetBytes(b, "n").Int())
			io.WriteString(w, `{"data":[{"task_id":"task_one"}]}`)
		case "/v1/tasks/task_one":
			polls.Add(1)
			io.WriteString(w, `{"data":{"status":"completed","result":{"images":[{"url":"https://files.example/result.png"}]}}}`)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	provider := &APIMart{BaseURL: upstream.URL + "/v1", Key: "supplier-key", Client: upstream.Client()}
	routes := map[string]Route{"apimart": {Provider: provider, AsyncProvider: provider, Token: "route-key", Models: map[string]bool{"gpt-image-2.5-sunburst": true}}, "other": {AsyncProvider: provider, Token: "other-key", Models: map[string]bool{"gpt-image-2.5-sunburst": true}}}
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	config := AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 2, MaxPending: 20, TaskTimeoutSeconds: 3600, RetentionHours: 24}
	manager, err := openAsyncManager(config, routes, client, time.Millisecond)
	require.NoError(t, err)
	server := &Server{Routes: routes, Slots: make(chan struct{}, 1), Async: manager}
	call := func(method, path, body, key string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		return w
	}
	payload := `{"job_id":"job-stable","model":"gpt-image-2.5-sunburst","prompt":"杯子","size":"1:1","resolution":"4k"}`
	for i := 0; i < 2; i++ {
		w := call("POST", "/apimart/v1/image-tasks", payload, "route-key")
		require.Equal(t, 202, w.Code, w.Body.String())
	}
	assert.Equal(t, 409, call("POST", "/apimart/v1/image-tasks", strings.ReplaceAll(payload, "杯子", "猫"), "route-key").Code)
	assert.Equal(t, 401, call("GET", "/other/v1/image-tasks/job-stable", "", "route-key").Code)
	assert.Equal(t, 404, call("GET", "/other/v1/image-tasks/job-stable", "", "other-key").Code)
	// 提交后模拟进程重启；恢复只查询已保存的上游任务 ID。
	require.NoError(t, manager.runOnce(context.Background()))
	assert.Equal(t, int32(1), submits.Load())
	require.NoError(t, manager.Close())
	manager, err = openAsyncManager(config, routes, client, time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	server.Async = manager
	require.NoError(t, manager.runOnce(context.Background()))
	require.NoError(t, manager.runOnce(context.Background()))
	w := call("GET", "/apimart/v1/image-tasks/job-stable", "", "route-key")
	require.Equal(t, 200, w.Code, w.Body.String())
	assert.Equal(t, "succeeded", gjson.GetBytes(w.Body.Bytes(), "status").String())
	assert.Equal(t, "2880x2880", gjson.GetBytes(w.Body.Bytes(), "actual_size").String())
	result := call("GET", "/apimart/v1/image-tasks/job-stable/result", "", "route-key")
	require.Equal(t, 200, result.Code)
	assert.Equal(t, picture, result.Body.Bytes())
	assert.Equal(t, "image/png", result.Header().Get("Content-Type"))
	assert.Equal(t, int32(1), submits.Load())
	assert.Equal(t, int32(1), polls.Load())
	// 相同 ID 在完成后仍返回同一任务，不再次计费。
	assert.Equal(t, 202, call("POST", "/apimart/v1/image-tasks", payload, "route-key").Code)
}

func TestAsyncUnknownSubmissionIsNeverRetried(t *testing.T) {
	var submits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { submits.Add(1); w.WriteHeader(500) }))
	defer upstream.Close()
	provider := &Kie{BaseURL: upstream.URL, Client: upstream.Client()}
	routes := map[string]Route{"kie": {AsyncProvider: provider, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	config := AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 5, TaskTimeoutSeconds: 3600, RetentionHours: 24}
	manager, err := openAsyncManager(config, routes, resultClient(), time.Millisecond)
	require.NoError(t, err)
	req := &AsyncRequest{JobID: "uncertain", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"}
	_, apiErr := manager.create("kie", req)
	require.Nil(t, apiErr)
	require.NoError(t, manager.runOnce(context.Background()))
	require.NoError(t, manager.Close())
	manager, err = openAsyncManager(config, routes, resultClient(), time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	require.NoError(t, manager.runOnce(context.Background()))
	task, err := manager.get("kie", "uncertain")
	require.NoError(t, err)
	assert.Equal(t, "unknown", task.Status)
	assert.False(t, task.Retryable)
	assert.Equal(t, int32(1), submits.Load())
	// 外部提交前落盘 submitting，崩溃没有 ID 时保守停止。
	raw, err := common.Marshal(req)
	require.NoError(t, err)
	require.NoError(t, manager.db.Create(&asyncTask{Key: asyncTaskKey("kie", "crash"), JobID: "crash", Route: "kie", Payload: raw, Status: "submitting", DeadlineAt: time.Now().Add(time.Hour).Unix()}).Error)
	require.NoError(t, manager.runOnce(context.Background()))
	task, err = manager.get("kie", "crash")
	require.NoError(t, err)
	assert.Equal(t, "unknown", task.Status)
	assert.False(t, task.Retryable)
}

func TestAsyncRejectsUnsupportedAndInvalidBeforeSubmission(t *testing.T) {
	provider := &Kie{}
	base := AsyncRequest{JobID: "job", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"}
	for _, tt := range []struct {
		name      string
		change    func(*AsyncRequest)
		code      string
		retryable bool
	}{
		{"quality", func(r *AsyncRequest) { r.Quality = "high" }, "unsupported_image_options", true},
		{"format", func(r *AsyncRequest) { r.OutputFormat = "png" }, "unsupported_image_options", true},
		{"ratio", func(r *AsyncRequest) { r.Size = "5:4" }, "unsupported_image_options", true},
		{"private", func(r *AsyncRequest) { r.ImageURLs = []string{"https://127.0.0.1/secret"} }, "invalid_request_error", false},
		{"credential", func(r *AsyncRequest) { r.ImageURLs = []string{"https://token@example.com/secret"} }, "invalid_request_error", false},
		{"broken", func(r *AsyncRequest) { r.ImageURLs = []string{"data:image/png;base64,YmFk"} }, "invalid_image_input", false},
		{"path", func(r *AsyncRequest) { r.JobID = "../escape" }, "invalid_request_error", false},
		{"mismatched family", func(r *AsyncRequest) { r.Resolution = "4k" }, "invalid_request_error", false},
		{"compression", func(r *AsyncRequest) { n := 101; r.OutputCompression = &n; r.OutputFormat = "jpeg" }, "invalid_request_error", false},
		{"mask", func(r *AsyncRequest) { r.MaskURL = "https://example.com/mask.png" }, "invalid_request_error", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			req := base
			tt.change(&req)
			e := provider.ValidateTask(&req)
			require.NotNil(t, e)
			assert.Equal(t, tt.code, e.Code)
			assert.Equal(t, tt.retryable, e.Retryable)
		})
	}
}

func TestAsyncDownloadRecoveryAndExpiryNeverRegenerates(t *testing.T) {
	var submits, downloads atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobs/createTask" {
			submits.Add(1)
			io.WriteString(w, `{"code":200,"data":{"taskId":"task_result"}}`)
			return
		}
		io.WriteString(w, `{"code":200,"data":{"taskId":"task_result","state":"success","resultJson":"{\"resultUrls\":[\"https://cdn.example/result.png\"]}"}}`)
	}))
	defer upstream.Close()
	provider := &Kie{BaseURL: upstream.URL, Client: upstream.Client()}
	routes := map[string]Route{"kie": {AsyncProvider: provider, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	picture := asyncPicture(t, 1254, 1254)
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("Authorization"))
		if downloads.Add(1) == 1 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("busy"))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	config := AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 2, TaskTimeoutSeconds: 3600, RetentionHours: 24}
	manager, err := openAsyncManager(config, routes, client, time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	request := &AsyncRequest{JobID: "recover", Model: "gpt-image-2.5-flare", Prompt: "杯子", Size: "1:1", Resolution: "1k"}
	_, e := manager.create("kie", request)
	require.Nil(t, e)
	for i := 0; i < 3; i++ {
		require.NoError(t, manager.runOnce(context.Background()))
	}
	task, err := manager.get("kie", "recover")
	require.NoError(t, err)
	assert.Equal(t, "running", task.Status)
	assert.False(t, task.Retryable)
	for i := 0; i < 2; i++ {
		require.NoError(t, manager.runOnce(context.Background()))
	}
	task, err = manager.get("kie", "recover")
	require.NoError(t, err)
	assert.Equal(t, "succeeded", task.Status)
	assert.Equal(t, "1254x1254", task.ActualSize)
	assert.Equal(t, int32(1), submits.Load())
	require.NoError(t, manager.db.Model(task).Update("updated_at", time.Now().Add(-25*time.Hour).Unix()).Error)
	require.NoError(t, manager.cleanup())
	task, err = manager.get("kie", "recover")
	require.NoError(t, err)
	assert.True(t, task.Expired)
	assert.Empty(t, task.Payload)
	_, e = manager.create("kie", request)
	require.Nil(t, e)
	server := &Server{Routes: routes, Async: manager}
	r := httptest.NewRequest("GET", "/kie/v1/image-tasks/recover/result", nil)
	r.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	assert.Equal(t, 410, w.Code)
	require.NoError(t, manager.runOnce(context.Background()))
	assert.Equal(t, int32(1), submits.Load())
}

func TestAsyncStoreLockCapacityAndClientCancellation(t *testing.T) {
	var submits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NoError(t, r.Context().Err())
		submits.Add(1)
		io.WriteString(w, `{"code":200,"data":{"taskId":"accepted"}}`)
	}))
	defer upstream.Close()
	provider := &Kie{BaseURL: upstream.URL, Client: upstream.Client()}
	routes := map[string]Route{"kie": {AsyncProvider: provider, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	config := AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}
	manager, err := openAsyncManager(config, routes, resultClient(), time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	_, err = openAsyncManager(config, routes, resultClient(), time.Millisecond)
	require.Error(t, err)
	server := &Server{Routes: routes, Async: manager, Slots: make(chan struct{}, 1)}
	server.Slots <- struct{}{}
	// 客户端断开也不取消已经持久受理的任务，且旧同步 slots 满不影响新队列。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := httptest.NewRequest("POST", "/kie/v1/image-tasks", strings.NewReader(`{"job_id":"one","model":"gpt-image-2.5-flare","prompt":"猫","resolution":"1k","size":"1:1"}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	require.Equal(t, 202, w.Code, w.Body.String())
	_, e := manager.create("kie", &AsyncRequest{JobID: "two", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"})
	require.NotNil(t, e)
	assert.Equal(t, 429, e.Status)
	assert.True(t, e.Retryable)
	require.NoError(t, manager.runOnce(context.Background()))
	assert.Equal(t, int32(1), submits.Load())
}

func TestKieSubmitRejectionsSeparateKnownFromUnknown(t *testing.T) {
	for _, tt := range []struct {
		status             int
		body               string
		retryable, unknown bool
	}{
		{429, `{}`, true, false}, {200, `{"code":429}`, true, false},
		{402, `{}`, true, false}, {403, `{}`, true, false}, {404, `{}`, true, false},
		{200, `{"code":401}`, true, false}, {200, `{"code":402}`, true, false}, {200, `{"code":403}`, true, false}, {200, `{"code":404}`, true, false},
		{400, `{}`, false, false}, {422, `{}`, false, false}, {200, `{"code":400}`, false, false}, {200, `{"code":422}`, false, false},
		{408, `{}`, false, true}, {402, `{"data":{"taskId":"invalid/id"}}`, false, true}, {500, `{}`, false, true}, {200, `{"code":500}`, false, true}, {200, `{"code":200,"data":{}}`, false, true}, {401, `{}`, true, false},
	} {
		t.Run(strconv.Itoa(tt.status)+tt.body, func(t *testing.T) {
			var submits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				submits.Add(1)
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer upstream.Close()
			p := &Kie{BaseURL: upstream.URL, Client: upstream.Client()}
			_, e := p.SubmitTask(context.Background(), &AsyncRequest{JobID: "job", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"})
			require.NotNil(t, e)
			assert.Equal(t, tt.retryable, e.Retryable)
			assert.Equal(t, tt.unknown, e.Unknown)
			assert.Equal(t, int32(1), submits.Load())
		})
	}
}

func TestKieCallbackVerifiesSignatureAndOnlyQueriesProvider(t *testing.T) {
	var polls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		polls.Add(1)
		io.WriteString(w, `{"code":200,"data":{"taskId":"upstream","state":"generating"}}`)
	}))
	defer upstream.Close()
	p := &Kie{BaseURL: upstream.URL, Client: upstream.Client(), CallbackURL: "https://callback.example/kie", WebhookKey: "webhook-key"}
	routes := map[string]Route{"kie": {AsyncProvider: p, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, resultClient(), time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	task, e := manager.create("kie", &AsyncRequest{JobID: "callback-job", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"})
	require.Nil(t, e)
	task.Status = "running"
	task.ProviderTaskID = "upstream"
	require.NoError(t, manager.save(task))
	server := &Server{Routes: routes, Async: manager}
	for _, scenario := range []string{"missing", "expired", "valid", "replay"} {
		r := httptest.NewRequest("POST", "/kie/v1/image-tasks/callback", strings.NewReader(`{"data":{"taskId":"upstream","state":"success","resultJson":"forged"}}`))
		timestamp := time.Now().Unix()
		if scenario == "expired" {
			timestamp -= 301
		}
		stamp := strconv.FormatInt(timestamp, 10)
		mac := hmac.New(sha256.New, []byte("webhook-key"))
		mac.Write([]byte("upstream." + stamp))
		r.Header.Set("X-Webhook-Timestamp", stamp)
		if scenario != "missing" {
			r.Header.Set("X-Webhook-Signature", base64.StdEncoding.EncodeToString(mac.Sum(nil)))
		}
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if scenario == "missing" || scenario == "expired" {
			assert.Equal(t, 401, w.Code)
		} else {
			assert.Equal(t, 200, w.Code)
		}
	}
	task, err = manager.get("kie", "callback-job")
	require.NoError(t, err)
	assert.Equal(t, "running", task.Status)
	assert.Empty(t, task.ResultURLs)
	require.NoError(t, manager.runOnce(context.Background()))
	assert.Equal(t, int32(1), polls.Load())
}

func TestAsyncHTTPRejectsUnknownAndBatchParameters(t *testing.T) {
	p := &Kie{}
	routes := map[string]Route{"kie": {AsyncProvider: p, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, resultClient(), time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	server := &Server{Routes: routes, Async: manager}
	for _, extra := range []string{`,"n":2`, `,"callBackUrl":"https://example.com"`, `,"bad":"value"`} {
		r := httptest.NewRequest("POST", "/kie/v1/image-tasks", strings.NewReader(`{"job_id":"job","model":"gpt-image-2.5-flare","prompt":"猫","size":"1:1","resolution":"1k"`+extra+`}`))
		r.Header.Set("Authorization", "Bearer key")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		assert.Equal(t, 400, w.Code)
	}
	var count int64
	require.NoError(t, manager.db.Model(&asyncTask{}).Count(&count).Error)
	assert.Zero(t, count)
	server.Async = nil
	r := httptest.NewRequest("POST", "/kie/v1/image-tasks", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	assert.Equal(t, 404, w.Code)
}

func TestAsyncConcurrentCreateCommitsOneTask(t *testing.T) {
	p := &Kie{}
	routes := map[string]Route{"kie": {AsyncProvider: p, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, resultClient(), time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	ready := make(chan struct{})
	out := make(chan *AsyncError, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-ready
			_, e := manager.create("kie", &AsyncRequest{JobID: "same", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"})
			out <- e
		}()
	}
	close(ready)
	require.Nil(t, <-out)
	require.Nil(t, <-out)
	var count int64
	require.NoError(t, manager.db.Model(&asyncTask{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestAsyncWorkerRunsIndependentlyOfHTTPAndStopsCleanly(t *testing.T) {
	submitted := make(chan struct{}, 1)
	finished := make(chan struct{}, 1)
	picture := asyncPicture(t, 1254, 1254)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/jobs/createTask" {
			submitted <- struct{}{}
			io.WriteString(w, `{"code":200,"data":{"taskId":"background"}}`)
			return
		}
		io.WriteString(w, `{"code":200,"data":{"taskId":"background","state":"success","resultJson":"{\"resultUrls\":[\"https://cdn.example/output.png\"]}"}}`)
	}))
	defer upstream.Close()
	p := &Kie{BaseURL: upstream.URL, Client: upstream.Client()}
	routes := map[string]Route{"kie": {AsyncProvider: p, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		finished <- struct{}{}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, client, time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	manager.Start()
	server := httptest.NewServer(&Server{Routes: routes, Async: manager})
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/kie/v1/image-tasks", strings.NewReader(`{"job_id":"background-job","model":"gpt-image-2.5-flare","prompt":"猫","size":"1:1","resolution":"1k"}`))
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer key")
	response, err := server.Client().Do(req)
	require.NoError(t, err)
	require.Equal(t, 202, response.StatusCode)
	response.Body.Close()
	cancel()
	select {
	case <-submitted:
	case <-time.After(30 * time.Second):
		t.Fatal("任务未提交")
	}
	select {
	case <-finished:
	case <-time.After(30 * time.Second):
		t.Fatal("客户端断开后任务未继续")
	}
	require.NoError(t, manager.Close())
	// Close 等待当前工作落盘；再次打开后仍有最终图片。
	manager2, err := openAsyncManager(manager.config, routes, client, time.Millisecond)
	require.NoError(t, err)
	defer manager2.Close()
	task, err := manager2.get("kie", "background-job")
	require.NoError(t, err)
	assert.Equal(t, "succeeded", task.Status)
}

func TestAsyncAcceptedInvalidResultsNeverPermitFallback(t *testing.T) {
	for _, scenario := range []string{"corrupt", "wrong_format", "wrong_native_resolution", "opaque"} {
		t.Run(scenario, func(t *testing.T) {
			picture := asyncPicture(t, 1024, 1024)
			if scenario == "corrupt" {
				picture = []byte("bad")
			}
			if scenario == "opaque" {
				var buf bytes.Buffer
				img := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
				for i := 3; i < len(img.Pix); i += 4 {
					img.Pix[i] = 255
				}
				require.NoError(t, png.Encode(&buf, img))
				picture = buf.Bytes()
			}
			provider := &APIMart{}
			routes := map[string]Route{"apimart": {AsyncProvider: provider, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true, "gpt-image-2.5-sunburst": true}}}
			client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture))}, nil
			})}
			manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, client, time.Millisecond)
			require.NoError(t, err)
			defer manager.Close()
			request := &AsyncRequest{JobID: "bad-result", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"}
			expected := "image_result_invalid"
			if scenario == "wrong_format" {
				request.OutputFormat = "jpeg"
				expected = "image_output_format_mismatch"
			}
			if scenario == "wrong_native_resolution" {
				request.Model = "gpt-image-2.5-sunburst"
				request.Resolution = "4k"
				expected = "image_resolution_mismatch"
			}
			if scenario == "opaque" {
				request.Background = "transparent"
				expected = "image_transparency_mismatch"
			}
			task, e := manager.create("apimart", request)
			require.Nil(t, e)
			task.Status = "downloading"
			task.ProviderTaskID = "task_accepted"
			task.ResultURLs = []byte(`["https://cdn.example/output.png"]`)
			require.NoError(t, manager.save(task))
			require.NoError(t, manager.runOnce(context.Background()))
			task, err = manager.get("apimart", "bad-result")
			require.NoError(t, err)
			assert.Equal(t, "failed", task.Status)
			assert.Equal(t, expected, task.ErrorCode)
			assert.False(t, task.Retryable)
		})
	}
}

func TestAsyncStorageRejectsSharedReadableDirectory(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "shared")
	require.NoError(t, os.Mkdir(directory, 0755))
	require.NoError(t, os.Chmod(directory, 0755))
	_, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: directory, Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, nil, resultClient(), time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "0700")
	_, err = os.Stat(filepath.Join(directory, "tasks.sqlite"))
	assert.True(t, os.IsNotExist(err))
}

func TestConfiguredAsyncIsOptInAndMissingWebhookKeyUsesPolling(t *testing.T) {
	t.Setenv("ASYNC_TEST_KIE_KEY", "supplier-key")
	t.Setenv("ASYNC_TEST_INTERNAL_KEY", strings.Repeat("k", 32))
	t.Setenv("ASYNC_TEST_WEBHOOK_KEY", "")
	config := Config{Listen: "127.0.0.1:8320", TimeoutSeconds: 270, PollIntervalMS: 100, MaxInFlight: 1, Async: AsyncConfig{DataDir: filepath.Join(t.TempDir(), "private"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, Providers: map[string]ProviderConfig{"kie": {Adapter: "kie", BaseURL: "https://api.kie.ai", APIKeyEnv: "ASYNC_TEST_KIE_KEY", AuthTokenEnv: "ASYNC_TEST_INTERNAL_KEY", Models: []string{"gpt-image-2.5-flare"}, AsyncEnabled: true, CallbackURL: "https://callback.example/kie", WebhookKeyEnv: "ASYNC_TEST_WEBHOOK_KEY"}}}
	server, err := configuredServer(config)
	require.NoError(t, err)
	assert.Nil(t, server.Async)
	_, err = os.Stat(config.Async.DataDir)
	assert.True(t, os.IsNotExist(err))
	config.Async.Enabled = true
	server, err = configuredServer(config)
	require.NoError(t, err)
	require.NotNil(t, server.Async)
	defer server.Async.Close()
	provider, ok := server.Routes["kie"].AsyncProvider.(*Kie)
	require.True(t, ok)
	assert.Empty(t, provider.WebhookKey)
	assert.Equal(t, "https://api.kie.ai", provider.UploadBaseURL)
	r := httptest.NewRequest("POST", "/kie/v1/images/generations", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer "+strings.Repeat("k", 32))
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	assert.Equal(t, 404, w.Code)
	r = httptest.NewRequest("POST", "/kie/v1/image-tasks/callback", strings.NewReader(`{}`))
	w = httptest.NewRecorder()
	server.ServeHTTP(w, r)
	assert.Equal(t, 404, w.Code)
}

func TestAsyncAllowsFortyMegapixelsButRejectsUnsafeDimensions(t *testing.T) {
	for _, tt := range []struct {
		width, height int
		allowed       bool
	}{
		{8000, 5000, true}, {8000, 5001, false}, {8193, 1000, false}, {0, 1024, false}, {1024, -1, false},
	} {
		assert.Equal(t, tt.allowed, validImageDimensions(image.Config{Width: tt.width, Height: tt.height}), "%dx%d", tt.width, tt.height)
	}
}

func TestAsyncResultAllows64MiBAndDoesNotDeliverLargerFiles(t *testing.T) {
	picture := make([]byte, 64<<20)
	copy(picture, asyncPicture(t, 1024, 1024))
	provider := &Kie{}
	routes := map[string]Route{"kie": {AsyncProvider: provider, Token: "key", Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture))}, nil
	})}
	manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, client, time.Millisecond)
	require.NoError(t, err)
	defer manager.Close()
	task, e := manager.create("kie", &AsyncRequest{JobID: "large-output", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"})
	require.Nil(t, e)
	task.Status = "downloading"
	task.ProviderTaskID = "upstream"
	task.ResultURLs = []byte(`["https://cdn.example/output.png"]`)
	require.NoError(t, manager.save(task))
	require.NoError(t, manager.runOnce(context.Background()))
	task, err = manager.get("kie", "large-output")
	require.NoError(t, err)
	require.Equal(t, "succeeded", task.Status)
	server := &Server{Routes: routes, Async: manager}
	r := httptest.NewRequest("GET", "/kie/v1/image-tasks/large-output/result", nil)
	r.Header.Set("Authorization", "Bearer key")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, r)
	require.Equal(t, 200, w.Code)
	assert.Len(t, w.Body.Bytes(), 64<<20)
	// 结果文件被异常扩长时拒绝交付；不能把超限文件当成功图片。
	f, err := os.OpenFile(filepath.Join(manager.config.DataDir, "results", task.Key), os.O_APPEND|os.O_WRONLY, 0600)
	require.NoError(t, err)
	_, err = f.Write([]byte{0})
	require.NoError(t, err)
	require.NoError(t, f.Close())
	w = httptest.NewRecorder()
	server.ServeHTTP(w, r)
	assert.Equal(t, 503, w.Code)
}

func TestKieNativeSizesAcceptVerifiedSamplesWithoutClosedAllowlist(t *testing.T) {
	for _, tt := range []struct {
		resolution    string
		width, height int
		accepted      bool
	}{
		{"1k", 1024, 1024, true}, {"1k", 1254, 1254, true}, {"1k", 1280, 1280, true}, {"2k", 2048, 2048, true}, {"4k", 2880, 2880, true},
		{"4k", 2048, 2048, false}, {"2k", 1024, 1024, false}, {"1k", 1024, 768, false},
	} {
		assert.Equal(t, tt.accepted, kieNativeSizeMatches("1:1", tt.resolution, tt.width, tt.height))
	}
	assert.True(t, kieNativeSizeMatches("16:9", "4k", 3840, 2160))
	assert.False(t, kieNativeSizeMatches("16:9", "4k", 3840, 2048))
	assert.False(t, kieNativeSizeMatches("3:1", "4k", 3000, 1000))
	assert.True(t, kieNativeSizeMatches("1:1", "2k", 2048, 2040))
	assert.False(t, kieNativeSizeMatches("1:1", "2k", 2048, 2000))
}

func TestAsyncInputsEnforceDecodedAndAggregateEncodedLimits(t *testing.T) {
	picture := make([]byte, 15<<20)
	copy(picture, asyncPicture(t, 16, 16))
	value := "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture)
	// 三张15MiB图片的编码正文连同Data URL前缀超过60MiB，在提交前拒绝。
	request := &AsyncRequest{JobID: "large-input", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k", ImageURLs: []string{value, value, value}}
	e := validateAsyncRequest(request)
	require.NotNil(t, e)
	assert.Equal(t, "invalid_request_error", e.Code)
	assert.Contains(t, e.Message, "总大小")
	// URL输入下载后遵守相同编码总量；两张20MiB与一张4MiB仍在60MiB内。
	twenty := make([]byte, 20<<20)
	copy(twenty, picture[:1024])
	four := make([]byte, 4<<20)
	copy(four, picture[:1024])
	client := &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		body := twenty
		if strings.HasSuffix(r.URL.Path, "small.png") {
			body = four
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(body))}, nil
	})}
	request.ImageURLs = []string{"https://cdn.example/a.png", "https://cdn.example/b.png", "https://cdn.example/small.png"}
	require.Nil(t, prepareAsyncInputs(context.Background(), client, request))
	// 蒙版独立4MiB上限必须在受理前生效。
	request.ImageURLs = []string{"https://cdn.example/a.png"}
	request.MaskURL = value
	e = validateAsyncRequest(request)
	require.NotNil(t, e)
	assert.Equal(t, "invalid_image_input", e.Code)
	// 单张参考图恰20MiB可读取，超1字节则拒绝。
	_, _, e = decodeImageInput("data:image/png;base64,"+base64.StdEncoding.EncodeToString(twenty), 20<<20)
	require.Nil(t, e)
	tooLarge := append(twenty, byte(0))
	_, _, e = decodeImageInput("data:image/png;base64,"+base64.StdEncoding.EncodeToString(tooLarge), 20<<20)
	require.NotNil(t, e)
}

func TestAsyncRejectedResponseWithTaskIDKeepsPollingInsteadOfFallback(t *testing.T) {
	for _, tt := range []struct {
		status     int
		body       string
		wantStatus string
		retryable  bool
	}{
		{402, `{"code":402}`, "failed", true},
		{200, `{"code":402}`, "failed", true},
		{402, `{"code":402,"data":{"taskId":"accepted-despite-error"}}`, "running", false},
		{200, `{"code":402,"data":{"taskId":"accepted-despite-error"}}`, "running", false},
		{500, `{"code":500,"data":{"taskId":"accepted-despite-error"}}`, "running", false},
	} {
		t.Run(strconv.Itoa(tt.status)+tt.body, func(t *testing.T) {
			var submits, polls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v1/jobs/createTask" {
					submits.Add(1)
					w.WriteHeader(tt.status)
					io.WriteString(w, tt.body)
					return
				}
				polls.Add(1)
				assert.Equal(t, "accepted-despite-error", r.URL.Query().Get("taskId"))
				io.WriteString(w, `{"code":200,"data":{"taskId":"accepted-despite-error","state":"generating"}}`)
			}))
			defer upstream.Close()
			p := &Kie{BaseURL: upstream.URL, Client: upstream.Client()}
			routes := map[string]Route{"kie": {AsyncProvider: p, Token: "key", Models: map[string]bool{"gpt-image-2.5-sunburst": true}}}
			manager, err := openAsyncManager(AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 1, TaskTimeoutSeconds: 3600, RetentionHours: 24}, routes, resultClient(), time.Millisecond)
			require.NoError(t, err)
			defer manager.Close()
			request := &AsyncRequest{JobID: "funds", Model: "gpt-image-2.5-sunburst", Prompt: "猫", Size: "1:1", Resolution: "4k"}
			_, e := manager.create("kie", request)
			require.Nil(t, e)
			require.NoError(t, manager.runOnce(context.Background()))
			task, err := manager.get("kie", "funds")
			require.NoError(t, err)
			assert.Equal(t, tt.wantStatus, task.Status)
			assert.Equal(t, tt.retryable, task.Retryable)
			if tt.wantStatus == "failed" {
				assert.Empty(t, task.ProviderTaskID)
			} else {
				assert.Equal(t, "accepted-despite-error", task.ProviderTaskID)
			}
			_, e = manager.create("kie", request)
			require.Nil(t, e)
			require.NoError(t, manager.runOnce(context.Background()))
			assert.Equal(t, int32(1), submits.Load())
			if tt.wantStatus == "running" {
				assert.Equal(t, int32(1), polls.Load())
			} else {
				assert.Zero(t, polls.Load())
			}
		})
	}
}

func TestAPIMartAsyncExplicitChannelRejectionPreservesAcceptanceEvidence(t *testing.T) {
	for _, tt := range []struct {
		status             int
		body               string
		wantID             string
		retryable, unknown bool
	}{
		{402, `{}`, "", true, false}, {200, `{"error":{"code":402}}`, "", true, false},
		{400, `{}`, "", false, false}, {500, `{}`, "", false, true},
		{402, `{"data":[{"task_id":"task_accepted"}]}`, "task_accepted", false, false},
		{500, `{"data":[{"task_id":"task_accepted"}]}`, "task_accepted", false, false},
		{402, `{"data":[{"task_id":"invalid/id"}]}`, "", false, true},
	} {
		t.Run(strconv.Itoa(tt.status)+tt.body, func(t *testing.T) {
			var submits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				submits.Add(1)
				w.WriteHeader(tt.status)
				io.WriteString(w, tt.body)
			}))
			defer upstream.Close()
			p := &APIMart{BaseURL: upstream.URL, Client: upstream.Client()}
			id, e := p.SubmitTask(context.Background(), &AsyncRequest{JobID: "job", Model: "gpt-image-2.5-flare", Prompt: "猫", Size: "1:1", Resolution: "1k"})
			assert.Equal(t, tt.wantID, id)
			if tt.wantID != "" {
				require.Nil(t, e)
			} else {
				require.NotNil(t, e)
				assert.Equal(t, tt.retryable, e.Retryable)
				assert.Equal(t, tt.unknown, e.Unknown)
			}
			assert.Equal(t, int32(1), submits.Load())
		})
	}
}

func TestAsyncUnknownSurvivesRetentionAndRestart(t *testing.T) {
	config := AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 1, MaxPending: 2, TaskTimeoutSeconds: 3600, RetentionHours: 24}
	routes := map[string]Route{"kie": {AsyncProvider: &Kie{}, Models: map[string]bool{"gpt-image-2.5-flare": true}}}
	manager, err := openAsyncManager(config, routes, &http.Client{}, time.Millisecond)
	require.NoError(t, err)
	request := &AsyncRequest{JobID: "unknown-retention", Model: "gpt-image-2.5-flare", Prompt: "杯子", Size: "1:1", Resolution: "1k"}
	task, apiErr := manager.create("kie", request)
	require.Nil(t, apiErr)
	require.NoError(t, manager.db.Model(task).Updates(map[string]any{"status": "unknown", "updated_at": time.Now().Add(-25 * time.Hour).Unix()}).Error)
	require.NoError(t, manager.cleanup())
	manager.Close()
	resumed, err := openAsyncManager(config, routes, &http.Client{}, time.Millisecond)
	require.NoError(t, err)
	defer resumed.Close()
	current, err := resumed.get("kie", request.JobID)
	require.NoError(t, err)
	assert.Equal(t, "unknown", current.Status)
	assert.False(t, current.Expired)
	assert.NotEmpty(t, current.Payload)
	replay, e := resumed.create("kie", request)
	require.Nil(t, e)
	assert.Equal(t, current.Key, replay.Key)
	assert.Equal(t, "unknown", replay.Status)
}
