package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"net/http"
	"os"
	"path/filepath"

	"github.com/QuantumNous/new-api/common"
)

// SubmitTask runs outside the shared task loop. Its durable submitting
// state is written before HTTP, so a restart can never replay the generation.
type Sub2API struct {
	BaseURL, Key  string
	Client        *http.Client
	MaxPerHour    int
	MaxConcurrent int
	route         string
	manager       *AsyncManager
}

func (p *Sub2API) ValidateTask(r *AsyncRequest) *AsyncError {
	ratio := r.Size
	if ratio == "" {
		ratio = "auto"
	}
	if r.Resolution != "1k" || len(r.ImageURLs) != 0 || r.MaskURL != "" || r.Quality != "" || (r.OutputFormat != "" && r.OutputFormat != "png") || r.OutputCompression != nil || r.Background == "transparent" || !common.Sub2APIImageRatio(ratio) {
		return asyncError(400, "unsupported_image_options", "本渠道仅支持 1K PNG 文生图及标准比例", true)
	}
	return validateAsyncRequest(r)
}

func (p *Sub2API) SubmitTask(ctx context.Context, r *AsyncRequest) (string, *AsyncError) {
	if e := p.ValidateTask(r); e != nil {
		return "", e
	}
	prompt := r.Prompt
	if r.Size != "auto" {
		prompt += "\n画幅比例 " + r.Size + " 构图 / aspect ratio " + r.Size
	}
	raw, err := common.Marshal(map[string]any{"model": r.Model, "prompt": prompt, "size": r.Size, "n": 1})
	if err != nil || p.manager == nil {
		return "", asyncError(503, "sub2api_submit_rejected", "提交准备失败", true)
	}
	status, body, err := boundedRequest(ctx, p.Client, http.MethodPost, p.BaseURL+"/v1/images/generations", p.Key, "application/json", raw, 90<<20)
	if err != nil {
		return "", asyncError(502, "sub2api_submit_rejected", "订阅生成失败，可使用备用渠道", true)
	}
	switch status {
	case 401, 402, 403, 429:
		return "", asyncError(status, "sub2api_submit_rejected", "上游明确拒绝任务", true)
	}
	if status != http.StatusOK {
		return "", asyncError(502, "sub2api_submit_rejected", "订阅生成失败，可使用备用渠道", true)
	}
	var response struct {
		Data []struct {
			B64JSON string `json:"b64_json"`
		} `json:"data"`
	}
	if common.Unmarshal(body, &response) != nil || len(response.Data) != 1 {
		return "", asyncError(502, "sub2api_submit_rejected", "订阅生成失败，可使用备用渠道", true)
	}
	picture, err := base64.StdEncoding.Strict().DecodeString(response.Data[0].B64JSON)
	if err != nil || len(picture) == 0 || len(picture) > 64<<20 {
		return "", asyncError(502, "sub2api_submit_rejected", "上游图片无效", true)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(picture))
	if err != nil || !validImageDimensions(cfg) || cfg.Width < 64 || cfg.Height < 64 || (format != "png" && format != "jpeg" && format != "webp") {
		return "", asyncError(502, "sub2api_submit_rejected", "上游图片无效", true)
	}
	if _, _, err := image.Decode(bytes.NewReader(picture)); err != nil || (r.OutputFormat != "" && r.OutputFormat != format) {
		return "", asyncError(502, "sub2api_submit_rejected", "上游图片无效", true)
	}
	if !common.Sub2APIImageSizeMatches(r.Size, cfg.Width, cfg.Height) {
		return "", asyncError(502, "sub2api_ratio_mismatch", "上游图片比例不符", true)
	}
	id := asyncTaskKey(p.route, r.JobID)
	if err := p.manager.writeResult(id, picture); err != nil {
		return "", asyncError(502, "sub2api_submit_rejected", "订阅生成失败，可使用备用渠道", true)
	}
	return id, nil
}

func (p *Sub2API) PollTask(ctx context.Context, id string) (*TaskResult, *AsyncError) {
	if len(id) != 64 || !asyncIDPattern.MatchString(id) || p.manager == nil {
		return nil, submitUnknown()
	}
	if _, err := os.Stat(filepath.Join(p.manager.config.DataDir, "results", id)); err != nil {
		return nil, submitUnknown()
	}
	return &TaskResult{}, nil
}

func (p *Sub2API) concurrency() int {
	if p.MaxConcurrent <= 0 {
		return 2
	}
	return p.MaxConcurrent
}
