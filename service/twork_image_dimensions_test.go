package service

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/png"
	"testing"

	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkImageDimensions(t *testing.T) {
	for _, tc := range []struct {
		name          string
		width, height int
		size          string
		valid         bool
	}{
		{"方图2K", 2048, 2048, "2048x2048", true},
		{"同画幅允许低分辨率兜底", 1254, 1254, "2048x2048", true},
		{"拒绝缩略图", 512, 512, "2048x2048", false},
		{"拒绝竖图", 1024, 1536, "1024x1024", false},
		{"允许更大同画幅", 1254, 1254, "1024x1024", true},
		{"允许像素取整", 2752, 1536, "2048x1152", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b bytes.Buffer
			require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, tc.width, tc.height))))
			raw := []byte(fmt.Sprintf(`{"data":[{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(b.Bytes())))
			size, err := ValidateTworkImageDimensions(raw, false, &dto.ImageRequest{Size: tc.size})
			if tc.valid {
				require.NoError(t, err)
				assert.Equal(t, fmt.Sprintf("%dx%d", tc.width, tc.height), size)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestTworkImageDimensionsRejectsUnverifiedURLAndMissingRequirements(t *testing.T) {
	_, err := ValidateTworkImageDimensions([]byte(`{"data":[{"url":"https://example.com/image.png"}]}`), false, &dto.ImageRequest{Size: "2048x2048"})
	require.Error(t, err)
	_, err = ValidateTworkImageDimensions([]byte(`{"data":[]}`), false, &dto.ImageRequest{})
	require.Error(t, err)
}

func TestTworkGeminiDimensions(t *testing.T) {
	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2048, 2048))))
	raw := []byte(fmt.Sprintf(`{"candidates":[{"content":{"parts":[{"inlineData":{"mimeType":"image/png","data":%q}}]}}]}`, base64.StdEncoding.EncodeToString(b.Bytes())))
	req := &dto.GeminiChatRequest{GenerationConfig: dto.GeminiChatGenerationConfig{ImageConfig: []byte(`{"aspectRatio":"1:1","imageSize":"2K"}`)}}
	size, err := ValidateTworkImageDimensions(raw, true, req)
	require.NoError(t, err)
	assert.Equal(t, "2048x2048", size)
	req.GenerationConfig.ImageConfig = []byte(`{"aspectRatio":"16:9","imageSize":"2K"}`)
	_, err = ValidateTworkImageDimensions(raw, true, req)
	require.Error(t, err)
}

func TestTworkImageDimensionsChecksEveryReturnedImage(t *testing.T) {
	var good, bad bytes.Buffer
	require.NoError(t, png.Encode(&good, image.NewRGBA(image.Rect(0, 0, 2048, 2048))))
	require.NoError(t, png.Encode(&bad, image.NewRGBA(image.Rect(0, 0, 1024, 1536))))
	raw := []byte(fmt.Sprintf(`{"data":[{"b64_json":%q},{"b64_json":%q}]}`, base64.StdEncoding.EncodeToString(good.Bytes()), base64.StdEncoding.EncodeToString(bad.Bytes())))
	_, err := ValidateTworkImageDimensions(raw, false, &dto.ImageRequest{Size: "2048x2048"})
	require.Error(t, err, "不能因第一张合格而结算包含错误尺寸的响应")
}
