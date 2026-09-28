package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"
)

// Provider 只处理供应商协议；轮询、取消和交付由 Engine 统一管理。
type Provider interface {
	Submit(context.Context, *ImageRequest) (string, *APIError)
	Poll(context.Context, string) (*TaskResult, *APIError)
}

type ImageRequest struct {
	Model             string                  `json:"model"`
	Prompt            string                  `json:"prompt"`
	Size              string                  `json:"size"`
	Quality           string                  `json:"quality"`
	N                 *int                    `json:"n"`
	Stream            bool                    `json:"stream"`
	ResponseFormat    string                  `json:"response_format"`
	Resolution        json.RawMessage         `json:"resolution"`
	OutputFormat      json.RawMessage         `json:"output_format"`
	OutputCompression json.RawMessage         `json:"output_compression"`
	Background        json.RawMessage         `json:"background"`
	Moderation        json.RawMessage         `json:"moderation"`
	Edit              bool                    `json:"-"`
	Images            []*multipart.FileHeader `json:"-"`
	Mask              *multipart.FileHeader   `json:"-"`
}

type TaskResult struct {
	Pending bool
	URLs    []string
	Usage   json.RawMessage
}

type ImageResult struct {
	Created int64               `json:"created"`
	Data    []map[string]string `json:"data"`
	Usage   json.RawMessage     `json:"usage,omitempty"`
}

type APIError struct {
	Status    int    `json:"-"`
	Code      string `json:"code"`
	Type      string `json:"type"`
	Message   string `json:"message"`
	Transient bool   `json:"-"`
}

func apiError(status int, code, message string) *APIError {
	return &APIError{Status: status, Code: code, Type: code, Message: message}
}

func unknown(message string) *APIError {
	// 400 防止旧网关按 5xx 重新提交；业务错误码使 Twork 同样停止跨渠道重试。
	return apiError(400, "image_result_invalid", message)
}

type Engine struct {
	Timeout        time.Duration
	PollInterval   time.Duration
	DownloadClient *http.Client
}

func (e *Engine) Generate(ctx context.Context, provider Provider, request *ImageRequest, accepted func(string)) (*ImageResult, *APIError) {
	ctx, cancel := context.WithTimeout(ctx, e.Timeout)
	defer cancel()
	taskID, err := provider.Submit(ctx, request)
	if err != nil {
		return nil, err
	}
	accepted(taskID)
	for {
		pollCtx, pollCancel := context.WithTimeout(ctx, 15*time.Second)
		task, pollErr := provider.Poll(pollCtx, taskID)
		pollCancel()
		if ctx.Err() != nil {
			return nil, unknown("等待已取消或超时，未重复提交任务")
		}
		if pollErr != nil && !pollErr.Transient {
			return nil, pollErr
		}
		if pollErr == nil && task != nil && !task.Pending {
			return e.download(ctx, task, request)
		}
		select {
		case <-ctx.Done():
			return nil, unknown("等待已取消或超时，未重复提交任务")
		case <-time.After(e.PollInterval):
		}
	}
}

func (e *Engine) download(ctx context.Context, task *TaskResult, request *ImageRequest) (*ImageResult, *APIError) {
	n := 1
	if request.N != nil {
		n = *request.N
	}
	if len(task.URLs) == 0 || len(task.URLs) > n {
		return nil, unknown("任务返回的图片数量无效")
	}
	result := &ImageResult{Created: time.Now().Unix(), Data: make([]map[string]string, 0, len(task.URLs)), Usage: task.Usage}
	remaining := int64(64 << 20)
	for _, target := range task.URLs {
		if !validResultURL(target) {
			return nil, unknown("图片结果地址无效")
		}
		var picture []byte
		var readErr error
		status := 0
		for attempt := 0; attempt < 3; attempt++ {
			status, picture, readErr = boundedRequest(ctx, e.DownloadClient, http.MethodGet, target, "", "", nil, remaining)
			if readErr == nil && status == 200 {
				break
			}
			if ctx.Err() != nil || readErr == nil && status != 408 && status != 429 && status < 500 {
				break
			}
			if attempt < 2 {
				select {
				case <-ctx.Done():
					return nil, unknown("下载已取消或超时")
				case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
				}
			}
		}
		if readErr != nil || status != 200 {
			return nil, unknown("图片下载失败，未重新生成")
		}
		config, _, decodeErr := image.DecodeConfig(bytes.NewReader(picture))
		if decodeErr != nil || config.Width <= 0 || config.Height <= 0 || config.Width > 8192 || config.Height > 8192 || int64(config.Width)*int64(config.Height) > 32*1024*1024 {
			return nil, unknown("图片格式或像素数量无效")
		}
		if _, _, decodeErr = image.Decode(bytes.NewReader(picture)); decodeErr != nil {
			return nil, unknown("图片损坏，未重新生成")
		}
		if ctx.Err() != nil {
			return nil, unknown("等待已取消或超时")
		}
		remaining -= int64(len(picture))
		result.Data = append(result.Data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(picture)})
	}
	return result, nil
}

func boundedRequest(ctx context.Context, client *http.Client, method, target, key, contentType string, body []byte, limit int64) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if int64(len(data)) > limit {
		return resp.StatusCode, nil, fmt.Errorf("响应超过大小限制")
	}
	return resp.StatusCode, data, err
}

func validResultURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

func publicAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, block := range []string{"100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001:db8::/32", "64:ff9b::/96", "64:ff9b:1::/48"} {
		if netip.MustParsePrefix(block).Contains(ip) {
			return false
		}
	}
	return true
}

func resultClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 30 * time.Second
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("结果地址无法解析")
		}
		for _, ip := range ips {
			if !publicAddress(ip) {
				return nil, fmt.Errorf("禁止访问非公网地址")
			}
		}
		dialer := net.Dialer{Timeout: 10 * time.Second}
		// 连接已验证的 IP，避免校验后再次 DNS 解析。
		var last error
		for _, ip := range ips {
			conn, e := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
			last = e
		}
		return nil, last
	}
	return &http.Client{Transport: transport, Timeout: 60 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !validResultURL(req.URL.String()) {
			return fmt.Errorf("图片重定向无效")
		}
		return nil
	}}
}
