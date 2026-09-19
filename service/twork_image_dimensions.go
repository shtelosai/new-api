package service

import (
	"encoding/base64"
	"errors"
	"fmt"
	"image"
	"math"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/dto"
	"github.com/tidwall/gjson"
)

// ValidateTworkImageDimensions 在结算前检查实际图片，不能用请求参数冒充实际尺寸。
// 比例允许最多 1% 像素取整；分辨率允许回退到 1K，不裁剪、不插值放大。
func ValidateTworkImageDimensions(body []byte, gemini bool, request dto.Request) (string, error) {
	var ratio float64
	var minEdge int
	switch req := request.(type) {
	case *dto.ImageRequest:
		parts := strings.Split(req.Size, "x")
		if len(parts) == 2 {
			w, e1 := strconv.Atoi(parts[0])
			h, e2 := strconv.Atoi(parts[1])
			if e1 == nil && e2 == nil && w > 0 && h > 0 && w <= 8192 && h <= 8192 {
				ratio = float64(w) / float64(h)
				minEdge = max(w, h)
			}
		}
	case *dto.GeminiChatRequest:
		config := req.GenerationConfig.ImageConfig
		aspect := gjson.GetBytes(config, "aspectRatio").String()
		if aspect == "" {
			aspect = gjson.GetBytes(config, "aspect_ratio").String()
		}
		parts := strings.Split(aspect, ":")
		if len(parts) == 2 {
			w, e1 := strconv.Atoi(parts[0])
			h, e2 := strconv.Atoi(parts[1])
			if e1 == nil && e2 == nil && w > 0 && h > 0 && w <= 21 && h <= 21 {
				ratio = float64(w) / float64(h)
			}
		}
		size := gjson.GetBytes(config, "imageSize").String()
		if size == "" {
			size = gjson.GetBytes(config, "image_size").String()
		}
		minEdge = map[string]int{"1K": 1024, "2K": 2048, "4K": 3840}[size]
	}
	if ratio == 0 || minEdge == 0 {
		return "", errors.New("图片请求缺少有效的尺寸要求")
	}
	encoded := []string{}
	if gemini {
		for _, candidate := range gjson.GetBytes(body, "candidates").Array() {
			for _, part := range candidate.Get("content.parts").Array() {
				data := part.Get("inlineData.data")
				if !data.Exists() {
					data = part.Get("inline_data.data")
				}
				if data.Exists() {
					encoded = append(encoded, data.String())
				}
			}
		}
	} else {
		for _, item := range gjson.GetBytes(body, "data").Array() {
			// URL 未下载无法证明像素尺寸，不能先扣费再交给后端发现不合格。
			encoded = append(encoded, item.Get("b64_json").String())
		}
	}
	if len(encoded) == 0 {
		return "", errors.New("图片渠道没有返回可核验尺寸的内联图片")
	}
	actual := ""
	for _, value := range encoded {
		config, _, err := image.DecodeConfig(base64.NewDecoder(base64.StdEncoding, strings.NewReader(value)))
		if err != nil || config.Width <= 0 || config.Height <= 0 {
			return "", errors.New("图片渠道没有返回可核验尺寸的内联图片")
		}
		if math.Abs(float64(config.Width)/float64(config.Height)/ratio-1) > 0.01 || max(config.Width, config.Height) < 1024 {
			return "", fmt.Errorf("图片尺寸不符合要求：实际 %dx%d，要求比例 %.4g、长边至少 1024 像素", config.Width, config.Height, ratio)
		}
		if actual == "" {
			actual = fmt.Sprintf("%dx%d", config.Width, config.Height)
		}
	}
	return actual, nil
}
