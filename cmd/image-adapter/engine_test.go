package main

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type testProvider struct {
	submits, polls int
	poll           func(context.Context, string) (*TaskResult, *APIError)
}

func (p *testProvider) Submit(context.Context, *ImageRequest) (string, *APIError) {
	p.submits++
	return "task_test", nil
}
func (p *testProvider) Poll(ctx context.Context, id string) (*TaskResult, *APIError) {
	p.polls++
	return p.poll(ctx, id)
}

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEnginePollsSameTaskAndReturnsActualImages(t *testing.T) {
	var picture bytes.Buffer
	require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1024, 1024))))
	provider := &testProvider{}
	provider.poll = func(ctx context.Context, id string) (*TaskResult, *APIError) {
		require.Equal(t, "task_test", id)
		if provider.polls == 1 {
			return nil, &APIError{Transient: true}
		}
		return &TaskResult{URLs: []string{"https://images.example/test.png"}}, nil
	}
	// 此用例验证轮询和交付，不以墙钟时间作为验收；race/并发构建下保留足够解码预算。
	engine := &Engine{Timeout: 30 * time.Second, PollInterval: time.Millisecond, DownloadClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		require.Empty(t, r.Header.Get("Authorization"))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture.Bytes()))}, nil
	})}}
	n := 2
	result, err := engine.Generate(context.Background(), provider, &ImageRequest{N: &n}, func(string) {})
	require.Nil(t, err)
	require.Len(t, result.Data, 1)
	require.Equal(t, 1, provider.submits)
	require.Equal(t, 2, provider.polls)
}

func TestEngineNeverResubmitsAfterCancellationOrBadResult(t *testing.T) {
	for _, scenario := range []string{"cancel", "broken"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &testProvider{}
			provider.poll = func(context.Context, string) (*TaskResult, *APIError) {
				if scenario == "cancel" {
					cancel()
					return &TaskResult{Pending: true}, nil
				}
				return &TaskResult{URLs: []string{"https://images.example/bad.png"}}, nil
			}
			engine := &Engine{Timeout: time.Second, PollInterval: time.Millisecond, DownloadClient: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("bad"))}, nil
			})}}
			_, err := engine.Generate(ctx, provider, &ImageRequest{}, func(string) {})
			require.NotNil(t, err)
			require.Equal(t, "image_result_invalid", err.Code)
			require.Equal(t, 400, err.Status)
			require.Equal(t, 1, provider.submits)
		})
	}
}

func TestEngineRetriesDownloadWithoutCreatingAnotherTask(t *testing.T) {
	var picture bytes.Buffer
	require.NoError(t, png.Encode(&picture, image.NewRGBA(image.Rect(0, 0, 1024, 1024))))
	provider := &testProvider{poll: func(context.Context, string) (*TaskResult, *APIError) {
		return &TaskResult{URLs: []string{"https://images.example/test.png"}}, nil
	}}
	downloads := 0
	engine := &Engine{Timeout: 30 * time.Second, PollInterval: time.Millisecond, DownloadClient: &http.Client{Transport: testTransport(func(*http.Request) (*http.Response, error) {
		downloads++
		if downloads == 1 {
			return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader("busy"))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(picture.Bytes()))}, nil
	})}}
	result, err := engine.Generate(context.Background(), provider, &ImageRequest{}, func(string) {})
	require.Nil(t, err)
	require.Len(t, result.Data, 1)
	require.Equal(t, 1, provider.submits)
	require.Equal(t, 2, downloads)
}
