package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

// Kie 的同步入口沿用既有轮询引擎，受理后超时或下载失败都不能再次提交。
func (p *Kie) Submit(ctx context.Context, request *ImageRequest) (string, *APIError) {
	if request.Stream || request.N != nil && *request.N != 1 || request.ResponseFormat != "" && request.ResponseFormat != "b64_json" {
		return "", invalid("Kie 同步入口仅支持单张非流式图片")
	}
	if request.Mask != nil || request.Quality != "" || len(request.OutputCompression) > 0 || len(request.Moderation) > 0 {
		return "", apiError(503, "unsupported_image_options", "本渠道未提交：不支持指定图片参数")
	}
	r := &AsyncRequest{JobID: "legacy", Model: request.Model, Prompt: request.Prompt, Size: request.Size, Resolution: "1k"}
	for _, field := range []struct {
		raw    []byte
		target *string
	}{{request.Resolution, &r.Resolution}, {request.OutputFormat, &r.OutputFormat}, {request.Background, &r.Background}} {
		if len(field.raw) > 0 && common.Unmarshal(field.raw, field.target) != nil {
			return "", invalid("图片参数类型无效")
		}
	}
	if strings.Contains(r.Size, "x") {
		var width, height int
		if _, err := fmt.Sscanf(r.Size, "%dx%d", &width, &height); err != nil || width <= 0 || height <= 0 || width > 8192 || height > 8192 {
			return "", invalid("图片尺寸无效")
		}
		r.Size = ""
		for _, ratio := range []string{"1:1", "3:2", "2:3", "4:3", "3:4", "16:9", "9:16", "21:9"} {
			var w, h int
			_, _ = fmt.Sscanf(ratio, "%d:%d", &w, &h)
			if math.Abs(float64(width)/float64(height)/(float64(w)/float64(h))-1) <= 0.01 {
				r.Size = ratio
				break
			}
		}
		if r.Size == "" {
			return "", apiError(503, "unsupported_image_options", "本渠道未提交：不支持指定画幅")
		}
	}
	if request.Edit {
		if len(request.Images) == 0 || len(request.Images) > 16 {
			return "", invalid("编辑需要 1–16 张参考图")
		}
		for _, file := range request.Images {
			if file.Size <= 0 || file.Size > 20<<20 {
				return "", invalid("参考图大小无效")
			}
			handle, err := file.Open()
			if err != nil {
				return "", invalid("无法读取参考图")
			}
			data, err := io.ReadAll(io.LimitReader(handle, (20<<20)+1))
			handle.Close()
			if err != nil || len(data) > 20<<20 {
				return "", invalid("参考图读取失败")
			}
			_, format, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || (format != "png" && format != "jpeg" && format != "webp") {
				return "", invalid("参考图格式无效")
			}
			r.ImageURLs = append(r.ImageURLs, "data:image/"+format+";base64,"+base64.StdEncoding.EncodeToString(data))
		}
	}
	id, err := p.SubmitTask(ctx, r)
	if err == nil {
		return id, nil
	}
	if err.Unknown {
		return "", unknown("任务提交结果未知，请勿重复生成")
	}
	if err.Retryable {
		return "", apiError(503, err.Code, "上游明确未受理图片任务")
	}
	return "", apiError(400, err.Code, err.Message)
}

func (p *Kie) Poll(ctx context.Context, id string) (*TaskResult, *APIError) {
	result, err := p.PollTask(ctx, id)
	if err == nil {
		return result, nil
	}
	if err.Transient {
		return nil, &APIError{Status: 503, Code: err.Code, Transient: true}
	}
	return nil, unknown("图片任务未能交付，未重新生成")
}

// Kie 不提供格式选择；按原尺寸编码交付文件，不改变像素尺寸或重新生图。
func encodeKieOutput(data []byte, decoded image.Image, sourceFormat, target string) ([]byte, string, error) {
	if target == "" || target == sourceFormat {
		return data, sourceFormat, nil
	}
	var out bytes.Buffer
	var err error
	switch target {
	case "png":
		err = png.Encode(&out, decoded)
	case "jpeg":
		err = jpeg.Encode(&out, decoded, &jpeg.Options{Quality: 95})
	default:
		return nil, "", fmt.Errorf("不支持的交付格式")
	}
	if err != nil {
		return nil, "", err
	}
	if out.Len() > 64<<20 {
		return nil, "", fmt.Errorf("图片编码超过交付上限")
	}
	return out.Bytes(), target, nil
}
