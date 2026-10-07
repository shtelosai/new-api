package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"image"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/netip"
	"net/textproto"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// AsyncRequest 与旧同步协议分离；档位始终以供应商原生参数发送。
type AsyncRequest struct {
	JobID             string   `json:"job_id"`
	Model             string   `json:"model"`
	Prompt            string   `json:"prompt"`
	Size              string   `json:"size"`
	Resolution        string   `json:"resolution"`
	OutputFormat      string   `json:"output_format,omitempty"`
	OutputCompression *int     `json:"output_compression,omitempty"`
	Background        string   `json:"background,omitempty"`
	Quality           string   `json:"quality,omitempty"`
	ImageURLs         []string `json:"image_urls,omitempty"`
	MaskURL           string   `json:"mask_url,omitempty"`
}

type AsyncError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
	Status    int    `json:"-"`
	Unknown   bool   `json:"-"`
	Transient bool   `json:"-"`
}

func asyncError(status int, code, message string, retryable bool) *AsyncError {
	return &AsyncError{Status: status, Code: code, Message: message, Retryable: retryable}
}
func submitUnknown() *AsyncError {
	return &AsyncError{Status: 502, Code: "image_submission_unknown", Message: "提交结果未知，禁止重复生成", Unknown: true}
}

type AsyncProvider interface {
	ValidateTask(*AsyncRequest) *AsyncError
	SubmitTask(context.Context, *AsyncRequest) (string, *AsyncError)
	PollTask(context.Context, string) (*TaskResult, *AsyncError)
}

var asyncIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)
var providerIDPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,160}$`)
var asyncRatios = map[string]bool{"auto": true, "1:1": true, "3:2": true, "2:3": true, "4:3": true, "3:4": true, "5:4": true, "4:5": true, "16:9": true, "9:16": true, "2:1": true, "1:2": true, "21:9": true, "9:21": true, "3:1": true, "1:3": true, "27:16": true, "16:27": true, "9:8": true, "8:9": true}
var kieRatios = map[string]bool{"auto": true, "1:1": true, "3:2": true, "2:3": true, "4:3": true, "3:4": true, "16:9": true, "9:16": true, "21:9": true, "27:16": true, "16:27": true, "9:8": true, "8:9": true}

func validateAsyncRequest(r *AsyncRequest) *AsyncError {
	if !asyncIDPattern.MatchString(r.JobID) || strings.TrimSpace(r.Prompt) == "" || utf8.RuneCountInString(r.Prompt) > 20000 || !utf8.ValidString(r.Prompt) {
		return asyncError(400, "invalid_request_error", "任务 ID 或提示词无效", false)
	}
	if r.Model != "gpt-image-2.5-flare" && r.Model != "gpt-image-2.5-sunburst" {
		return asyncError(400, "invalid_request_error", "异步接口仅支持 GPT Image 2.5", false)
	}
	if r.Size == "" {
		r.Size = "auto"
	}
	if !asyncRatios[r.Size] {
		return asyncError(400, "invalid_request_error", "异步接口须使用支持的比例", false)
	}
	if r.Resolution != "1k" && r.Resolution != "2k" && r.Resolution != "4k" {
		return asyncError(400, "invalid_request_error", "分辨率档位无效", false)
	}
	if r.Resolution == "1k" && r.Model != "gpt-image-2.5-flare" || r.Resolution != "1k" && r.Model != "gpt-image-2.5-sunburst" {
		return asyncError(400, "invalid_request_error", "1K 使用 Flare，2K/4K 使用 Sunburst", false)
	}
	if r.OutputFormat == "auto" {
		r.OutputFormat = ""
	}
	if r.OutputFormat != "" && r.OutputFormat != "png" && r.OutputFormat != "jpeg" && r.OutputFormat != "webp" {
		return asyncError(400, "invalid_request_error", "输出格式无效", false)
	}
	if r.OutputCompression != nil && (*r.OutputCompression < 0 || *r.OutputCompression > 100 || (r.OutputFormat != "jpeg" && r.OutputFormat != "webp")) {
		return asyncError(400, "invalid_request_error", "压缩参数仅支持 JPEG/WebP 的 0–100", false)
	}
	if r.Background != "" && r.Background != "transparent" && r.Background != "opaque" && r.Background != "auto" {
		return asyncError(400, "invalid_request_error", "背景参数无效", false)
	}
	if r.Background == "transparent" && r.OutputFormat == "jpeg" {
		return asyncError(400, "invalid_request_error", "JPEG 不支持透明背景", false)
	}
	if r.Quality != "" && !strings.Contains("|auto|low|medium|high|xhigh|max|", "|"+r.Quality+"|") {
		return asyncError(400, "invalid_request_error", "质量档位无效", false)
	}
	if len(r.ImageURLs) > 16 || r.MaskURL != "" && len(r.ImageURLs) == 0 {
		return asyncError(400, "invalid_request_error", "最多 16 张参考图，蒙版须配合参考图使用", false)
	}
	total := 0
	inputs := append([]string{}, r.ImageURLs...)
	if r.MaskURL != "" {
		inputs = append(inputs, r.MaskURL)
	}
	for i, input := range inputs {
		total += len(input)
		if total > 60<<20 {
			return asyncError(400, "invalid_request_error", "参考图总大小超过限制", false)
		}
		if strings.HasPrefix(input, "data:") {
			limit := 20 << 20
			if i == len(r.ImageURLs) {
				limit = 4 << 20
			}
			if _, _, e := decodeImageInput(input, limit); e != nil {
				return e
			}
		} else if !validPublicImageURL(input) {
			return asyncError(400, "invalid_request_error", "参考图仅支持图片 Data URL 或公网 HTTPS", false)
		}
	}
	return nil
}

func validPublicImageURL(value string) bool {
	if len(value) > 8192 || !validResultURL(value) {
		return false
	}
	u, err := url.Parse(value)
	if err != nil || u.Fragment != "" || u.Hostname() == "localhost" || strings.HasSuffix(u.Hostname(), ".localhost") {
		return false
	}
	if u.Port() != "" && u.Port() != "443" {
		return false
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !publicAddress(ip) {
		return false
	}
	return true
}

func decodeImageInput(value string, limit int) ([]byte, string, *AsyncError) {
	prefix, encoded, ok := strings.Cut(value, ",")
	mime := strings.TrimSuffix(strings.TrimPrefix(prefix, "data:"), ";base64")
	if !ok || !strings.HasSuffix(prefix, ";base64") || (mime != "image/png" && mime != "image/jpeg" && mime != "image/webp") || len(encoded) > base64.StdEncoding.EncodedLen(limit) {
		return nil, "", asyncError(400, "invalid_image_input", "图片 Data URL 无效或超过大小限制", false)
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) == 0 || len(data) > limit {
		return nil, "", asyncError(400, "invalid_image_input", "图片 Base64 无效或超过大小限制", false)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || !validImageDimensions(config) || "image/"+format != mime {
		return nil, "", asyncError(400, "invalid_image_input", "图片格式或像素无效", false)
	}
	if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
		return nil, "", asyncError(400, "invalid_image_input", "输入图片损坏", false)
	}
	return data, mime, nil
}

func validImageDimensions(config image.Config) bool {
	return config.Width > 0 && config.Height > 0 && config.Width <= 8192 && config.Height <= 8192 && int64(config.Width)*int64(config.Height) <= 40_000_000
}

// prepareAsyncInputs 将用户 URL 安全读取为图片，供应商只能得到已验证、重新上传的内容。
func prepareAsyncInputs(ctx context.Context, client *http.Client, r *AsyncRequest) *AsyncError {
	inputs := append([]string{}, r.ImageURLs...)
	if r.MaskURL != "" {
		inputs = append(inputs, r.MaskURL)
	}
	var first image.Config
	total := 0
	for i, input := range inputs {
		limit := 20 << 20
		if i == len(r.ImageURLs) {
			limit = 4 << 20
		}
		var data []byte
		var mime string
		if strings.HasPrefix(input, "data:") {
			var e *AsyncError
			data, mime, e = decodeImageInput(input, limit)
			if e != nil {
				return e
			}
		} else {
			if !validPublicImageURL(input) {
				return asyncError(400, "invalid_image_input", "参考图地址无效", false)
			}
			status, body, err := boundedRequest(ctx, client, http.MethodGet, input, "", "", nil, int64(limit))
			if err != nil || status != 200 {
				return asyncError(502, "image_input_unavailable", "参考图暂不可下载，尚未提交生成", true)
			}
			mime = http.DetectContentType(body)
			input = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(body)
			var e *AsyncError
			data, mime, e = decodeImageInput(input, limit)
			if e != nil {
				return e
			}
		}
		total += len("data:"+mime+";base64,") + base64.StdEncoding.EncodedLen(len(data))
		if total > 60<<20 {
			return asyncError(400, "invalid_image_input", "参考图总大小超过限制", false)
		}
		config, _, _ := image.DecodeConfig(bytes.NewReader(data))
		if i == 0 {
			first = config
		}
		if i == len(r.ImageURLs) {
			if mime != "image/png" || len(data) < 26 || (data[25] != 4 && data[25] != 6) || config.Width != first.Width || config.Height != first.Height {
				return asyncError(400, "invalid_image_mask", "蒙版须为带 Alpha 通道且与首张参考图同尺寸的 PNG", false)
			}
			r.MaskURL = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		} else {
			r.ImageURLs[i] = "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data)
		}
	}
	return nil
}

func (p *APIMart) ValidateTask(r *AsyncRequest) *AsyncError {
	if err := validateAsyncRequest(r); err != nil {
		return err
	}
	if _, err := apimartImageSize(r.Size); err != nil {
		return asyncError(400, "unsupported_image_options", "本渠道不支持此比例", true)
	}
	return nil
}

func (p *APIMart) SubmitTask(ctx context.Context, r *AsyncRequest) (string, *AsyncError) {
	if err := p.ValidateTask(r); err != nil {
		return "", err
	}
	payload := map[string]any{"model": r.Model, "prompt": r.Prompt, "size": r.Size, "resolution": r.Resolution, "n": 1}
	if r.OutputFormat != "" {
		payload["output_format"] = r.OutputFormat
	}
	if r.OutputCompression != nil {
		payload["output_compression"] = *r.OutputCompression
	}
	if r.Quality != "" {
		payload["quality"] = r.Quality
	}
	if r.Background != "" {
		payload["background"] = r.Background
	}
	urls := make([]string, 0, len(r.ImageURLs))
	for _, input := range r.ImageURLs {
		value, e := uploadAsyncInput(ctx, p.Client, p.BaseURL+"/uploads/images", p.Key, input, false)
		if e != nil {
			return "", e
		}
		urls = append(urls, value)
	}
	if len(urls) > 0 {
		payload["image_urls"] = urls
	}
	if r.MaskURL != "" {
		payload["mask_url"] = r.MaskURL
	}
	raw, _ := common.Marshal(payload)
	status, body, err := boundedRequest(ctx, p.Client, "POST", p.BaseURL+"/images/generations", p.Key, "application/json", raw, 1<<20)
	if err != nil {
		return "", submitUnknown()
	}
	taskID := gjson.GetBytes(body, "data.0.task_id")
	if taskID.String() != "" {
		if taskID.Type != gjson.String || !taskPattern.MatchString(taskID.String()) {
			return "", submitUnknown()
		}
		return taskID.String(), nil
	}
	if status >= 500 || status == 408 || status < 400 && status != 200 {
		return "", submitUnknown()
	}
	if status != 200 {
		return "", asyncError(status, "apimart_submit_rejected", "上游明确拒绝任务", unacceptedChannelUnavailable(status))
	}
	code := int(gjson.GetBytes(body, "error.code").Int())
	if code == 400 || code == 422 || unacceptedChannelUnavailable(code) {
		return "", asyncError(400, "apimart_submit_rejected", "上游明确拒绝任务", unacceptedChannelUnavailable(code))
	}
	return "", submitUnknown()

}
func (p *APIMart) PollTask(ctx context.Context, id string) (*TaskResult, *AsyncError) {
	result, e := p.Poll(ctx, id)
	if e == nil {
		return result, nil
	}
	return nil, &AsyncError{Code: e.Code, Message: e.Message, Transient: e.Transient, Unknown: e.Code != "apimart_task_failed" && !e.Transient}
}

type Kie struct {
	BaseURL, UploadBaseURL, Key string
	CallbackURL, WebhookKey     string
	Client                      *http.Client
}

func (p *Kie) ValidateTask(r *AsyncRequest) *AsyncError {
	if err := validateAsyncRequest(r); err != nil {
		return err
	}
	if !kieRatios[r.Size] || r.OutputFormat != "" || r.OutputCompression != nil || r.Quality != "" || r.MaskURL != "" {
		return asyncError(400, "unsupported_image_options", "本渠道不支持指定比例、格式、压缩、质量或蒙版参数", true)
	}
	return nil
}
func (p *Kie) SubmitTask(ctx context.Context, r *AsyncRequest) (string, *AsyncError) {
	if err := p.ValidateTask(r); err != nil {
		return "", err
	}
	family := strings.TrimPrefix(r.Model, "gpt-image-2.5-")
	model := "gpt-image-2-5-" + family + "-text-to-image"
	input := map[string]any{"prompt": r.Prompt, "aspect_ratio": r.Size, "resolution": strings.ToUpper(r.Resolution)}
	if r.Background != "" {
		input["background"] = r.Background
	}
	if len(r.ImageURLs) > 0 {
		model = "gpt-image-2-5-" + family + "-image-to-image"
		base := p.UploadBaseURL
		if base == "" {
			base = p.BaseURL
		}
		urls := make([]string, 0, len(r.ImageURLs))
		for _, ref := range r.ImageURLs {
			value, e := uploadAsyncInput(ctx, p.Client, base+"/api/file-stream-upload", p.Key, ref, true)
			if e != nil {
				return "", e
			}
			urls = append(urls, value)
		}
		input["input_urls"] = urls
	}
	payload := map[string]any{"model": model, "input": input}
	if p.CallbackURL != "" && p.WebhookKey != "" {
		payload["callBackUrl"] = p.CallbackURL
	}
	raw, _ := common.Marshal(payload)
	status, body, err := boundedRequest(ctx, p.Client, "POST", p.BaseURL+"/api/v1/jobs/createTask", p.Key, "application/json", raw, 1<<20)
	if err != nil {
		return "", submitUnknown()
	}
	taskID := gjson.GetBytes(body, "data.taskId")
	// 任务 ID 比响应错误码更能说明已经受理；保存原 ID 后仅查询，禁止切换渠道重生。
	if taskID.String() != "" {
		if taskID.Type != gjson.String || !providerIDPattern.MatchString(taskID.String()) {
			return "", submitUnknown()
		}
		return taskID.String(), nil
	}
	if status >= 500 || status == 408 || status < 400 && status != 200 {
		return "", submitUnknown()
	}
	if status != 200 {
		return "", asyncError(status, "kie_submit_rejected", "上游明确拒绝任务", unacceptedChannelUnavailable(status))
	}
	code := int(gjson.GetBytes(body, "code").Int())
	if code == 400 || code == 422 || unacceptedChannelUnavailable(code) {
		return "", asyncError(400, "kie_submit_rejected", "上游明确拒绝任务", unacceptedChannelUnavailable(code))
	}
	return "", submitUnknown()

}
func (p *Kie) PollTask(ctx context.Context, id string) (*TaskResult, *AsyncError) {
	if !providerIDPattern.MatchString(id) {
		return nil, &AsyncError{Code: "image_status_unknown", Unknown: true}
	}
	status, body, err := boundedRequest(ctx, p.Client, "GET", p.BaseURL+"/api/v1/jobs/recordInfo?taskId="+url.QueryEscape(id), p.Key, "", nil, 4<<20)
	if err != nil || status == 408 || status == 429 || status >= 500 {
		return nil, &AsyncError{Transient: true}
	}
	if status != 200 || gjson.GetBytes(body, "code").Int() != 200 || gjson.GetBytes(body, "data.taskId").String() != id {
		return nil, &AsyncError{Code: "image_status_unknown", Unknown: true}
	}
	switch gjson.GetBytes(body, "data.state").String() {
	case "waiting", "queuing", "generating":
		return &TaskResult{Pending: true}, nil
	case "fail":
		return nil, &AsyncError{Code: "kie_task_failed", Message: "上游图片任务失败"}
	case "success":
		raw := gjson.GetBytes(body, "data.resultJson").String()
		if !gjson.Valid(raw) {
			return nil, &AsyncError{Code: "image_result_invalid", Message: "上游结果无效"}
		}
		result := &TaskResult{}
		for _, u := range gjson.Get(raw, "resultUrls").Array() {
			result.URLs = append(result.URLs, u.String())
		}
		return result, nil
	default:
		return nil, &AsyncError{Code: "image_status_unknown", Unknown: true}
	}
}

// 上传失败发生在生成请求之前，可安全改用备用渠道；临时 URL 只用于同一次任务。
func uploadAsyncInput(ctx context.Context, client *http.Client, target, key, input string, kie bool) (string, *AsyncError) {
	data, mime, e := decodeImageInput(input, 20<<20)
	if e != nil {
		return "", e
	}
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp"}[mime]
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", asyncError(500, "image_upload_failed", "上传准备失败，尚未提交生成", true)
	}
	hash := sha256.Sum256(random)
	filename := fmt.Sprintf("%x%s", hash[:16], ext)
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="`+filename+`"`)
	header.Set("Content-Type", mime)
	part, err := form.CreatePart(header)
	if err != nil {
		return "", asyncError(500, "image_upload_failed", "上传准备失败，尚未提交生成", true)
	}
	if _, err = io.Copy(part, bytes.NewReader(data)); err != nil {
		return "", asyncError(500, "image_upload_failed", "上传准备失败，尚未提交生成", true)
	}
	if kie {
		if err = form.WriteField("uploadPath", "images/twork"); err == nil {
			err = form.WriteField("fileName", filename)
		}
	}
	if err == nil {
		err = form.Close()
	}
	if err != nil {
		return "", asyncError(500, "image_upload_failed", "上传准备失败，尚未提交生成", true)
	}
	status, response, err := boundedRequest(ctx, client, "POST", target, key, form.FormDataContentType(), body.Bytes(), 1<<20)
	if err != nil || status != 200 {
		return "", asyncError(502, "image_upload_failed", "参考图上传失败，尚未提交生成", true)
	}
	value := gjson.GetBytes(response, "url").String()
	if kie {
		if !gjson.GetBytes(response, "success").Bool() || gjson.GetBytes(response, "code").Int() != 200 {
			return "", asyncError(502, "image_upload_failed", "参考图上传失败，尚未提交生成", true)
		}
		value = gjson.GetBytes(response, "data.downloadUrl").String()
	}
	if !validPublicImageURL(value) {
		return "", asyncError(502, "image_upload_failed", "上传未返回有效地址，尚未提交生成", true)
	}
	return value, nil
}

// 异步资源下载继续使用已验证 IP 拨号，重定向也保留 HTTPS/443 限制。
func asyncResultClient() *http.Client {
	client := resultClient()
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validPublicImageURL(req.URL.String()) {
			return fmt.Errorf("图片重定向无效")
		}
		return nil
	}
	return client
}

// Kie 按档位下限与比例容差核对，真实样本不构成封闭尺寸白名单。
func kieNativeSizeMatches(ratio, resolution string, width, height int) bool {
	if width <= 0 || height <= 0 {
		return false
	}
	if ratio != "auto" {
		parts := strings.Split(ratio, ":")
		if len(parts) != 2 {
			return false
		}
		a, e1 := strconv.Atoi(parts[0])
		b, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || a <= 0 || b <= 0 || math.Abs((float64(width)/float64(height))/(float64(a)/float64(b))-1) > 0.01 {
			return false
		}
	}
	edge, area := max(width, height), int64(width)*int64(height)
	switch resolution {
	case "1k":
		return area >= 655360 && edge >= 1024
	case "2k":
		return edge >= 2048
	case "4k":
		return area >= 4900000 && edge >= 2880
	default:
		return false
	}
}

// 必须先确认没有任务 ID；这些明确拒绝表示渠道暂不可用，可使用同档备用渠道。
func unacceptedChannelUnavailable(code int) bool {
	switch code {
	case 401, 402, 403, 404, 429:
		return true
	default:
		return false
	}
}
