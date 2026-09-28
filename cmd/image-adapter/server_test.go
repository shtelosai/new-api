package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestHTTPRoutesGenerateAndEditThroughAPIMart(t *testing.T) {
	var picture bytes.Buffer
	require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1024, 1024))))
	var submitted, polled, uploaded atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "Bearer supplier-secret", r.Header.Get("Authorization"))
		switch r.URL.Path {
		case "/v1/uploads/images":
			require.NoError(t, r.ParseMultipartForm(1<<20))
			defer r.MultipartForm.RemoveAll()
			file, _, err := r.FormFile("file")
			require.NoError(t, err)
			defer file.Close()
			data, err := io.ReadAll(file)
			require.NoError(t, err)
			assert.Equal(t, picture.Bytes(), data)
			uploaded.Add(1)
			io.WriteString(w, `{"url":"https://cdn.example/input.png"}`)
		case "/v1/images/generations":
			body, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			assert.Equal(t, "medium", gjson.GetBytes(body, "quality").String())
			assert.Equal(t, "2880x2880", gjson.GetBytes(body, "size").String())
			if submitted.Add(1) == 2 {
				assert.Len(t, gjson.GetBytes(body, "image_urls").Array(), 2)
				assert.Equal(t, "https://cdn.example/input.png", gjson.GetBytes(body, "mask_url").String())
			}
			io.WriteString(w, `{"data":[{"task_id":"task_test"}]}`)
		case "/v1/tasks/task_test":
			if polled.Add(1) == 1 {
				w.WriteHeader(503)
				return
			}
			io.WriteString(w, `{"data":{"status":"completed","result":{"images":[{"url":["https://cdn.example/output.png"]}]},"usage":{"input_tokens":100,"output_tokens":200,"total_tokens":300}}}`)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer upstream.Close()
	engine := &Engine{Timeout: time.Second, PollInterval: time.Millisecond, DownloadClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		assert.Empty(t, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture.Bytes()))}, nil
	})}}
	server := &Server{Engine: engine, Routes: map[string]Route{"apimart": {Provider: &APIMart{BaseURL: upstream.URL + "/v1", Key: "supplier-secret", Client: upstream.Client()}, Token: "adapter-secret", Models: map[string]bool{"gpt-image-2.5-flare": true}}}, Slots: make(chan struct{}, 2)}
	for _, edit := range []bool{false, true} {
		var request *http.Request
		if edit {
			var body bytes.Buffer
			form := multipart.NewWriter(&body)
			for field, value := range map[string]string{"model": "gpt-image-2.5-flare", "prompt": "改成蓝色", "size": "3840x3840", "n": "2"} {
				require.NoError(t, form.WriteField(field, value))
			}
			for _, field := range []string{"image[]", "image[2]", "mask"} {
				part, err := form.CreateFormFile(field, "image.png")
				require.NoError(t, err)
				_, err = part.Write(picture.Bytes())
				require.NoError(t, err)
			}
			require.NoError(t, form.Close())
			request = httptest.NewRequest("POST", "/apimart/v1/images/edits", &body)
			request.Header.Set("Content-Type", form.FormDataContentType())
		} else {
			request = httptest.NewRequest("POST", "/apimart/v1/images/generations", strings.NewReader(`{"model":"gpt-image-2.5-flare","prompt":"茶壶","size":"3840x3840","n":2}`))
		}
		request.Header.Set("Authorization", "Bearer adapter-secret")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		require.Equal(t, 200, response.Code, response.Body.String())
		assert.Equal(t, int64(1), gjson.GetBytes(response.Body.Bytes(), "data.#").Int())
		assert.Equal(t, int64(300), gjson.GetBytes(response.Body.Bytes(), "usage.total_tokens").Int())
	}
	assert.Equal(t, int32(2), submitted.Load())
	assert.Equal(t, int32(3), uploaded.Load())
}

func TestHTTPRejectsUnauthorizedUnknownModelsAndCapacityBeforeSubmitting(t *testing.T) {
	provider := &testProvider{}
	server := &Server{Engine: &Engine{Timeout: time.Second}, Routes: map[string]Route{"first": {Provider: provider, Token: "first-key", Models: map[string]bool{"allowed": true}}, "second": {Provider: provider, Token: "second-key", Models: map[string]bool{"allowed": true}}}, Slots: make(chan struct{}, 1)}
	for _, tt := range []struct {
		path, key, model string
		full             bool
		status           int
	}{
		{"first", "wrong", "allowed", false, 401},
		{"second", "first-key", "allowed", false, 401},
		{"first", "first-key", "unknown", false, 400},
		{"first", "first-key", "allowed", true, 429},
	} {
		if tt.full {
			server.Slots <- struct{}{}
		}
		r := httptest.NewRequest("POST", "/"+tt.path+"/v1/images/generations", strings.NewReader(`{"model":"`+tt.model+`","prompt":"cat"}`))
		r.Header.Set("Authorization", "Bearer "+tt.key)
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		assert.Equal(t, tt.status, w.Code)
		if tt.full {
			<-server.Slots
		}
	}
	assert.Zero(t, provider.submits)
}

func TestAPIMartRejectsInvalidAndUnknownSubmissionWithoutRetry(t *testing.T) {
	for _, scenario := range []string{"zero", "stream", "format", "server_error", "missing_id"} {
		t.Run(scenario, func(t *testing.T) {
			var submits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				submits.Add(1)
				if scenario == "server_error" {
					w.WriteHeader(500)
				} else {
					io.WriteString(w, `{"data":[]}`)
				}
			}))
			defer upstream.Close()
			p := &APIMart{BaseURL: upstream.URL, Client: upstream.Client()}
			request := &ImageRequest{Prompt: "cat"}
			if scenario == "zero" {
				zero := 0
				request.N = &zero
			}
			if scenario == "stream" {
				request.Stream = true
			}
			if scenario == "format" {
				request.ResponseFormat = "url"
			}
			_, err := p.Submit(context.Background(), request)
			require.NotNil(t, err)
			assert.Equal(t, 400, err.Status)
			if scenario == "server_error" || scenario == "missing_id" {
				assert.Equal(t, "image_result_invalid", err.Code)
				assert.Equal(t, int32(1), submits.Load())
			} else {
				assert.Zero(t, submits.Load())
			}
		})
	}
}

func TestResultDownloadBlocksPrivateAddresses(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "::1", "10.1.2.3", "172.20.0.1", "169.254.169.254", "100.64.1.1", "::ffff:127.0.0.1", "fe80::1", "2001:db8::1"} {
		assert.False(t, publicAddress(netip.MustParseAddr(value)), value)
	}
	assert.True(t, publicAddress(netip.MustParseAddr("8.8.8.8")))
	_, err := resultClient().Get("https://127.0.0.1:1/image.png")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "非公网")
	assert.False(t, validResultURL("http://images.example/test.png"))
	assert.False(t, validResultURL("https://secret@images.example/test.png"))
}

func TestAPIMartSizePreservesRatioUnderPixelBudget(t *testing.T) {
	for _, tt := range []struct{ input, want string }{{"3840x3840", "2880x2880"}, {"3840x3072", "3216x2576"}, {"3840x2160", "3840x2160"}, {"2048x1152", "2048x1152"}, {"1824x1024", "1824x1024"}} {
		actual, err := apimartImageSize(tt.input)
		require.NoError(t, err)
		assert.Equal(t, tt.want, actual)
	}
}

func TestConfiguredRoutesKeepIndependentCredentialsAndModels(t *testing.T) {
	t.Setenv("IMAGE_TEST_UPSTREAM_A", "upstream-a")
	t.Setenv("IMAGE_TEST_UPSTREAM_B", "upstream-b")
	t.Setenv("IMAGE_TEST_INTERNAL_A", strings.Repeat("a", 32))
	t.Setenv("IMAGE_TEST_INTERNAL_B", strings.Repeat("b", 32))
	config := Config{Listen: "127.0.0.1:8320", TimeoutSeconds: 270, PollIntervalMS: 2000, MaxInFlight: 4, Providers: map[string]ProviderConfig{
		"first":  {Adapter: "apimart", BaseURL: "https://first.example/v1", APIKeyEnv: "IMAGE_TEST_UPSTREAM_A", AuthTokenEnv: "IMAGE_TEST_INTERNAL_A", Models: []string{"model-a"}},
		"second": {Adapter: "apimart", BaseURL: "https://second.example/v1", APIKeyEnv: "IMAGE_TEST_UPSTREAM_B", AuthTokenEnv: "IMAGE_TEST_INTERNAL_B", Models: []string{"model-b"}},
	}}
	server, err := configuredServer(config)
	require.NoError(t, err)
	assert.Equal(t, "upstream-a", server.Routes["first"].Provider.(*APIMart).Key)
	assert.Equal(t, "upstream-b", server.Routes["second"].Provider.(*APIMart).Key)
	assert.NotEqual(t, server.Routes["first"].Token, server.Routes["second"].Token)
	assert.False(t, server.Routes["first"].Models["model-b"])
	config.Listen = "100.88.103.99:8320"
	_, err = configuredServer(config)
	require.NoError(t, err, "允许 Tailscale 私网监听")
	config.Listen = "0.0.0.0:8320"
	_, err = configuredServer(config)
	require.Error(t, err)
}
