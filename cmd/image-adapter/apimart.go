package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"regexp"
	"strconv"
	"strings"
)

var taskPattern = regexp.MustCompile(`^task_[a-zA-Z0-9_-]{1,128}$`)

type APIMart struct {
	BaseURL, Key string
	Client       *http.Client
}

func invalid(message string) *APIError { return apiError(400, "invalid_request_error", message) }

func (p *APIMart) Submit(ctx context.Context, request *ImageRequest) (string, *APIError) {
	n := 1
	if request.N != nil {
		n = *request.N
	}
	if n < 1 || n > 4 || request.Stream {
		return "", invalid("图片数量须为 1–4，暂不支持流式输出")
	}
	if request.Prompt == "" {
		return "", invalid("缺少图片提示词")
	}
	if request.ResponseFormat != "" && request.ResponseFormat != "b64_json" {
		return "", invalid("仅支持 b64_json 图片结果")
	}
	size, err := apimartImageSize(request.Size)
	if err != nil {
		return "", invalid(err.Error())
	}
	quality := request.Quality
	if quality == "" {
		quality = "medium"
	}
	if !strings.Contains("|auto|low|medium|high|xhigh|max|", "|"+quality+"|") {
		return "", invalid("图片质量档位无效")
	}
	payload := map[string]any{"model": request.Model, "prompt": request.Prompt, "size": size, "quality": quality, "n": n}
	for field, value := range map[string]json.RawMessage{"resolution": request.Resolution, "output_format": request.OutputFormat, "output_compression": request.OutputCompression, "background": request.Background, "moderation": request.Moderation} {
		if len(value) > 0 {
			payload[field] = value
		}
	}
	if request.Edit {
		if len(request.Images) < 1 || len(request.Images) > 16 {
			return "", invalid("编辑需要 1–16 张参考图")
		}
		files := append([]*multipart.FileHeader{}, request.Images...)
		if request.Mask != nil {
			files = append(files, request.Mask)
		}
		for i, file := range files {
			limit := int64(20 << 20)
			if i >= len(request.Images) {
				limit = 4 << 20
			}
			if file.Size <= 0 || file.Size > limit {
				return "", invalid("参考图或蒙版超过大小限制")
			}
		}
		urls := make([]string, 0, len(request.Images))
		for i, file := range files {
			value, uploadErr := p.upload(ctx, file)
			if uploadErr != nil {
				return "", uploadErr
			}
			if i < len(request.Images) {
				urls = append(urls, value)
			} else {
				payload["mask_url"] = value
			}
		}
		payload["image_urls"] = urls
	}
	raw, err := common.Marshal(payload)
	if err != nil {
		return "", invalid("图片参数无法编码")
	}
	status, body, err := boundedRequest(ctx, p.Client, "POST", p.BaseURL+"/images/generations", p.Key, "application/json", raw, 1<<20)
	if err != nil || status < 400 && status != 200 || status >= 500 || status == 408 {
		return "", unknown("任务提交结果未知，请勿重复生成")
	}
	if status != 200 {
		return "", apiError(status, "apimart_submit_rejected", fmt.Sprintf("上游拒绝任务（HTTP %d）", status))
	}
	id := gjson.GetBytes(body, "data.0.task_id").String()
	if !taskPattern.MatchString(id) {
		return "", unknown("上游未返回有效任务 ID")
	}
	return id, nil
}

func (p *APIMart) Poll(ctx context.Context, id string) (*TaskResult, *APIError) {
	if !taskPattern.MatchString(id) {
		return nil, unknown("任务 ID 无效")
	}
	status, body, err := boundedRequest(ctx, p.Client, "GET", p.BaseURL+"/tasks/"+id, p.Key, "", nil, 4<<20)
	if err != nil || status == 429 || status == 408 || status >= 500 {
		return nil, &APIError{Transient: true}
	}
	if status != 200 {
		return nil, unknown("任务状态暂不可确认")
	}
	switch gjson.GetBytes(body, "data.status").String() {
	case "pending", "processing", "submitted":
		return &TaskResult{Pending: true}, nil
	case "failed", "cancelled":
		return nil, apiError(400, "apimart_task_failed", "上游图片任务失败或取消")
	case "completed":
		result := &TaskResult{}
		for _, item := range gjson.GetBytes(body, "data.result.images").Array() {
			value := item.Get("url")
			if value.Type == gjson.String {
				result.URLs = append(result.URLs, value.String())
			} else {
				for _, u := range value.Array() {
					result.URLs = append(result.URLs, u.String())
				}
			}
		}
		if usage := gjson.GetBytes(body, "data.usage"); usage.IsObject() {
			result.Usage = json.RawMessage(usage.Raw)
		}
		return result, nil
	default:
		return nil, unknown("任务状态无效")
	}
}

func (p *APIMart) upload(ctx context.Context, file *multipart.FileHeader) (string, *APIError) {
	f, err := file.Open()
	if err != nil {
		return "", invalid("无法读取参考图片")
	}
	defer f.Close()
	prefix := make([]byte, 512)
	count, err := f.Read(prefix)
	if err != nil && err != io.EOF {
		return "", invalid("无法读取参考图片")
	}
	mime := http.DetectContentType(prefix[:count])
	ext, ok := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}[mime]
	if !ok {
		return "", invalid("参考图仅支持 PNG、JPEG、WebP")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", invalid("无法读取参考图片")
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="input`+ext+`"`)
	header.Set("Content-Type", mime)
	part, err := w.CreatePart(header)
	if err != nil {
		return "", invalid("无法组装上传")
	}
	if _, err = io.Copy(part, io.LimitReader(f, (20<<20)+1)); err != nil {
		return "", invalid("无法读取参考图片")
	}
	if err = w.Close(); err != nil {
		return "", invalid("无法组装上传")
	}
	status, data, err := boundedRequest(ctx, p.Client, "POST", p.BaseURL+"/uploads/images", p.Key, w.FormDataContentType(), body.Bytes(), 1<<20)
	if err != nil || status != 200 {
		return "", apiError(502, "image_upload_failed", "参考图上传失败，尚未提交生图任务")
	}
	value := gjson.GetBytes(data, "url").String()
	if !validResultURL(value) {
		return "", apiError(502, "image_upload_failed", "上传未返回有效 URL，尚未提交生图任务")
	}
	return value, nil
}

func apimartImageSize(size string) (string, error) {
	if size == "" || size == "auto" {
		return "auto", nil
	}
	if strings.Contains(size, ":") {
		for _, ratio := range []string{"1:1", "3:2", "2:3", "4:3", "3:4", "5:4", "4:5", "16:9", "9:16", "2:1", "1:2", "21:9", "9:21", "3:1", "1:3"} {
			if size == ratio {
				return size, nil
			}
		}
		return "", fmt.Errorf("APIMart 图片比例无效")
	}
	parts := strings.Split(size, "x")
	if len(parts) != 2 {
		return "", fmt.Errorf("APIMart 图片尺寸无效")
	}
	w, e1 := strconv.Atoi(parts[0])
	h, e2 := strconv.Atoi(parts[1])
	if e1 != nil || e2 != nil || w < 16 || h < 16 || w > 8192 || h > 8192 || w%16 != 0 || h%16 != 0 || max(w, h) > 3*min(w, h) || w*h < 655360 {
		return "", fmt.Errorf("APIMart 图片尺寸超出支持范围")
	}
	// 保持画幅，仅缩减超过上游单边/总像素上限的请求，不对输出图片插值。
	scale := math.Min(1, math.Min(3840/float64(max(w, h)), math.Sqrt(8294400/float64(w*h))))
	if scale < 1 {
		width, height := float64(w)*scale/16, float64(h)*scale/16
		w, h = int(math.Round(width))*16, int(math.Round(height))*16
		if w*h > 8294400 {
			w, h = int(math.Floor(width))*16, int(math.Floor(height))*16
		}
	}
	return fmt.Sprintf("%dx%d", w, h), nil
}
