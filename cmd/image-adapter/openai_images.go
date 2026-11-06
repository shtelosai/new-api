package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

const (
	openAIImagesTimeout   = 170 * time.Second
	openAIImagesRetention = 30 * time.Minute
)

var openAIImagesModelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,47}$`)

// inlineResultProvider 由同步返回图片的上游实现：结果在内存里，不经过下载地址。
type inlineResultProvider interface {
	InlineResult(id string) ([]byte, bool)
}

type openAIImagesJob struct {
	done      bool
	picture   []byte
	failure   *AsyncError
	startedAt time.Time
}

// OpenAIImages 对接只支持同步返回的 OpenAI 兼容图片接口。
// 上游要等几十秒，SubmitTask 只启动后台请求，避免占住任务循环。
type OpenAIImages struct {
	BaseURL string
	Key     string
	Client  *http.Client

	mu   sync.Mutex
	jobs map[string]*openAIImagesJob
}

func (p *OpenAIImages) ValidateTask(r *AsyncRequest) *AsyncError {
	if !openAIImagesModelPattern.MatchString(r.Model) {
		return asyncError(400, "invalid_request_error", "图片模型标识无效", false)
	}
	if e := validateAsyncFields(r); e != nil {
		return e
	}
	if _, ok := apimartNativeSize(r.Size, r.Resolution); !ok {
		return asyncError(400, "unsupported_image_options", "本渠道不支持此比例", true)
	}
	return nil
}

func (p *OpenAIImages) SubmitTask(_ context.Context, r *AsyncRequest) (string, *AsyncError) {
	if e := p.ValidateTask(r); e != nil {
		return "", e
	}
	request := *r
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.jobs == nil {
		p.jobs = map[string]*openAIImagesJob{}
	}
	for id, job := range p.jobs {
		if time.Since(job.startedAt) > openAIImagesRetention {
			delete(p.jobs, id)
		}
	}
	if _, exists := p.jobs[r.JobID]; !exists {
		p.jobs[r.JobID] = &openAIImagesJob{startedAt: time.Now()}
		go p.run(&request)
	}
	return r.JobID, nil
}

func (p *OpenAIImages) run(r *AsyncRequest) {
	ctx, cancel := context.WithTimeout(context.Background(), openAIImagesTimeout)
	defer cancel()
	picture, failure := p.generate(ctx, r)
	p.mu.Lock()
	defer p.mu.Unlock()
	if job := p.jobs[r.JobID]; job != nil {
		job.done, job.picture, job.failure = true, picture, failure
	}
}

func (p *OpenAIImages) generate(ctx context.Context, r *AsyncRequest) ([]byte, *AsyncError) {
	size, _ := apimartNativeSize(r.Size, r.Resolution)
	target, contentType := p.BaseURL+"/images/generations", "application/json"
	var body []byte
	if len(r.ImageURLs) > 0 {
		form, formType, e := openAIImagesEditForm(r, size)
		if e != nil {
			return nil, e
		}
		target, contentType, body = p.BaseURL+"/images/edits", formType, form
	} else {
		payload := map[string]any{"model": r.Model, "prompt": r.Prompt, "size": size, "n": 1}
		if r.Quality != "" {
			payload["quality"] = r.Quality
		}
		body, _ = common.Marshal(payload)
	}
	status, response, err := boundedRequest(ctx, p.Client, http.MethodPost, target, p.Key, contentType, body, 96<<20)
	if err != nil {
		return nil, &AsyncError{Code: "openai_images_failed", Message: "上游请求失败"}
	}
	if status != 200 {
		return nil, &AsyncError{Code: "openai_images_rejected", Message: "上游拒绝任务"}
	}
	encoded := gjson.GetBytes(response, "data.0.b64_json").String()
	picture, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(picture) == 0 {
		return nil, &AsyncError{Code: "image_result_invalid", Message: "上游未返回图片"}
	}
	return picture, nil
}

func openAIImagesEditForm(r *AsyncRequest, size string) ([]byte, string, *AsyncError) {
	var buffer bytes.Buffer
	form := multipart.NewWriter(&buffer)
	fields := map[string]string{"model": r.Model, "prompt": r.Prompt, "size": size, "n": "1"}
	for key, value := range fields {
		if form.WriteField(key, value) != nil {
			return nil, "", asyncError(400, "invalid_image_input", "参考图无效", false)
		}
	}
	add := func(field, value string, limit int) *AsyncError {
		data, mime, e := decodeImageInput(value, limit)
		if e != nil {
			return e
		}
		name := "input." + strings.TrimPrefix(mime, "image/")
		part, err := form.CreateFormFile(field, name)
		if err == nil {
			_, err = part.Write(data)
		}
		if err != nil {
			return asyncError(400, "invalid_image_input", "参考图无效", false)
		}
		return nil
	}
	for _, input := range r.ImageURLs {
		if e := add("image[]", input, 20<<20); e != nil {
			return nil, "", e
		}
	}
	if r.MaskURL != "" {
		if e := add("mask", r.MaskURL, 4<<20); e != nil {
			return nil, "", e
		}
	}
	if form.Close() != nil {
		return nil, "", asyncError(400, "invalid_image_input", "参考图无效", false)
	}
	return buffer.Bytes(), form.FormDataContentType(), nil
}

func (p *OpenAIImages) PollTask(_ context.Context, id string) (*TaskResult, *AsyncError) {
	p.mu.Lock()
	defer p.mu.Unlock()
	job := p.jobs[id]
	if job == nil {
		// 进程重启后结果已丢，按失败退款，不重新生成。
		return nil, &AsyncError{Code: "openai_images_lost", Message: "图片结果已丢失"}
	}
	if !job.done {
		return &TaskResult{Pending: true}, nil
	}
	if job.failure != nil {
		return nil, job.failure
	}
	return &TaskResult{Inline: true}, nil
}

func (p *OpenAIImages) InlineResult(id string) ([]byte, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	job := p.jobs[id]
	if job == nil || len(job.picture) == 0 {
		return nil, false
	}
	return job.picture, true
}
