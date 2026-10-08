package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKieLegacySunburstReturnsRequestedPNGWithoutRegenerating(t *testing.T) {
	var submits atomic.Int32
	var photo bytes.Buffer
	im := image.NewRGBA(image.Rect(0, 0, 1024, 1024))
	im.Set(0, 0, color.RGBA{R: 200, A: 255})
	require.NoError(t, jpeg.Encode(&photo, im, nil))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/jobs/createTask":
			submits.Add(1)
			var body map[string]any
			require.NoError(t, common.DecodeJson(r.Body, &body))
			assert.Equal(t, "gpt-image-2-5-sunburst-text-to-image", body["model"])
			input := body["input"].(map[string]any)
			assert.Equal(t, "1K", input["resolution"])
			assert.Equal(t, "1:1", input["aspect_ratio"])
			assert.NotContains(t, input, "output_format")
			io.WriteString(w, `{"code":200,"data":{"taskId":"sunburst-task"}}`)
		case "/api/v1/jobs/recordInfo":
			io.WriteString(w, `{"code":200,"data":{"taskId":"sunburst-task","state":"success","resultJson":"{\"resultUrls\":[\"https://images.example/result.jpg\"]}"}}`)
		default:
			t.Errorf("意外请求 %s", r.URL.Path)
		}
	}))
	defer upstream.Close()
	provider := &Kie{BaseURL: upstream.URL, Key: "test-only", Client: upstream.Client()}
	engine := &Engine{Timeout: time.Second, DownloadClient: &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(photo.Bytes()))}, nil
	})}}
	result, err := engine.Generate(context.Background(), provider, &ImageRequest{
		Model: "gpt-image-2.5-sunburst", Prompt: "杯子", Size: "1024x1024", Resolution: []byte(`"1k"`), OutputFormat: []byte(`"png"`),
	}, func(id string) { assert.Equal(t, "sunburst-task", id) })
	require.Nil(t, err)
	require.Len(t, result.Data, 1)
	data, decodeErr := base64.StdEncoding.DecodeString(result.Data[0]["b64_json"])
	require.NoError(t, decodeErr)
	cfg, format, decodeErr := image.DecodeConfig(bytes.NewReader(data))
	require.NoError(t, decodeErr)
	assert.Equal(t, "png", format)
	assert.Equal(t, 1024, cfg.Width)
	assert.Equal(t, 1024, cfg.Height)
	assert.Equal(t, int32(1), submits.Load())
}

func TestKieOutputEncodingPreservesAlphaAndDimensions(t *testing.T) {
	input := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	input.SetNRGBA(0, 0, color.NRGBA{R: 100, A: 80})
	data, format, err := encodeKieOutput(nil, input, "webp", "png")
	require.NoError(t, err)
	decoded, _, err := image.Decode(bytes.NewReader(data))
	require.NoError(t, err)
	assert.Equal(t, "png", format)
	assert.Equal(t, input.Bounds(), decoded.Bounds())
	assert.Equal(t, color.NRGBAModel.Convert(input.At(0, 0)), color.NRGBAModel.Convert(decoded.At(0, 0)))
}
