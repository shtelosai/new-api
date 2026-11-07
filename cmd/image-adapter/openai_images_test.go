package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func openAIImagesServer(t *testing.T, picture []byte, delay time.Duration, status int) (*OpenAIImages, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(delay)
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString(picture)}}})
	}))
	t.Cleanup(server.Close)
	return &OpenAIImages{BaseURL: server.URL + "/v1", Key: "k", Client: server.Client()}, &calls
}

func openAIImagesRequest() *AsyncRequest {
	return &AsyncRequest{JobID: "job-1", Model: "gpt-image-2.6-flare", Prompt: "杯子", Size: "16:9", Resolution: "1k"}
}

func waitOpenAIImages(t *testing.T, p *OpenAIImages) (*TaskResult, *AsyncError) {
	t.Helper()
	var result *TaskResult
	var failure *AsyncError
	require.Eventually(t, func() bool {
		result, failure = p.PollTask(context.Background(), "job-1")
		return failure != nil || !result.Pending
	}, 2*time.Second, 5*time.Millisecond)
	return result, failure
}

func TestOpenAIImagesSubmitDoesNotBlockTaskLoop(t *testing.T) {
	p, _ := openAIImagesServer(t, asyncPicture(t, 64, 64), 300*time.Millisecond, 200)
	started := time.Now()
	id, e := p.SubmitTask(context.Background(), openAIImagesRequest())
	require.Nil(t, e)
	assert.Less(t, time.Since(started), 100*time.Millisecond)
	result, failure := p.PollTask(context.Background(), id)
	require.Nil(t, failure)
	assert.True(t, result.Pending)
}

func TestOpenAIImagesReturnsInlinePictureOnce(t *testing.T) {
	picture := asyncPicture(t, 64, 64)
	p, calls := openAIImagesServer(t, picture, 0, 200)
	_, e := p.SubmitTask(context.Background(), openAIImagesRequest())
	require.Nil(t, e)
	_, e = p.SubmitTask(context.Background(), openAIImagesRequest())
	require.Nil(t, e)
	result, failure := waitOpenAIImages(t, p)
	require.Nil(t, failure)
	assert.True(t, result.Inline)
	data, ok := p.InlineResult("job-1")
	require.True(t, ok)
	assert.Equal(t, picture, data)
	assert.Equal(t, int32(1), calls.Load(), "重复提交不能重复扣上游费用")
}

func TestOpenAIImagesFailureIsTerminal(t *testing.T) {
	p, _ := openAIImagesServer(t, nil, 0, 400)
	_, e := p.SubmitTask(context.Background(), openAIImagesRequest())
	require.Nil(t, e)
	_, failure := waitOpenAIImages(t, p)
	require.NotNil(t, failure)
	assert.Equal(t, "openai_images_rejected", failure.Code)
}

func TestOpenAIImagesLostResultFailsInsteadOfRegenerating(t *testing.T) {
	p, calls := openAIImagesServer(t, nil, 0, 200)
	_, failure := p.PollTask(context.Background(), "gone")
	require.NotNil(t, failure)
	assert.Equal(t, "openai_images_lost", failure.Code)
	assert.Zero(t, calls.Load())
}

func TestOpenAIImagesRejectsUnsafeModelAndKeepsLegacyModelCheck(t *testing.T) {
	r := openAIImagesRequest()
	r.Model = "../other"
	assert.NotNil(t, (&OpenAIImages{}).ValidateTask(r))
	legacy := openAIImagesRequest()
	assert.Nil(t, validateAsyncRequest(legacy), "对话生图异步入口接受 GPT Image 2.6")
	legacy.Model = "gpt-image-2.5-flare"
	assert.Nil(t, validateAsyncRequest(legacy))
	legacy.Model = "gemini-3-pro-image"
	assert.NotNil(t, validateAsyncRequest(legacy), "异步入口只接受 GPT Image 2.5/2.6 家族")
}

func TestOpenAIImagesRetriesTransientUpstreamFailure(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": base64.StdEncoding.EncodeToString([]byte("png"))}}})
	}))
	t.Cleanup(server.Close)
	p := &OpenAIImages{BaseURL: server.URL + "/v1", Key: "k", Client: server.Client()}
	_, e := p.SubmitTask(context.Background(), openAIImagesRequest())
	require.Nil(t, e)
	require.Eventually(t, func() bool {
		result, failure := p.PollTask(context.Background(), "job-1")
		return failure == nil && !result.Pending
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(2), calls.Load())
}

func TestOpenAIImagesDoesNotRetryRejection(t *testing.T) {
	p, calls := openAIImagesServer(t, nil, 0, 400)
	_, e := p.SubmitTask(context.Background(), openAIImagesRequest())
	require.Nil(t, e)
	_, failure := waitOpenAIImages(t, p)
	require.NotNil(t, failure)
	assert.Equal(t, int32(1), calls.Load())
}

func TestOpenAIImagesCompletesThroughAsyncManager(t *testing.T) {
	picture := asyncPicture(t, 1024, 1024)
	provider, calls := openAIImagesServer(t, picture, 0, 200)
	routes := map[string]Route{"supergpt": {AsyncProvider: provider, Token: "route-key", Models: map[string]bool{"gpt-image-2.6-flare": true}}}
	config := AsyncConfig{Enabled: true, DataDir: filepath.Join(t.TempDir(), "tasks"), Workers: 2, MaxPending: 20, TaskTimeoutSeconds: 3600, RetentionHours: 24}
	manager, err := openAsyncManager(config, routes, provider.Client, time.Millisecond)
	require.NoError(t, err)
	_, e := manager.create("supergpt", openAIImagesRequest())
	require.Nil(t, e)
	var task *asyncTask
	require.Eventually(t, func() bool {
		require.NoError(t, manager.runOnce(context.Background()))
		task, err = manager.get("supergpt", "job-1")
		require.NoError(t, err)
		return task.Status == "succeeded" || task.Status == "failed"
	}, 3*time.Second, 5*time.Millisecond)
	assert.Equal(t, "succeeded", task.Status, task.ErrorCode)
	assert.Equal(t, "1024x1024", task.ActualSize)
	assert.Equal(t, 1, task.ImageCount)
	assert.Equal(t, int32(1), calls.Load())
}
