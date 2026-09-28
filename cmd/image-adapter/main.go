package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
)

var revision = "dev"

type ProviderConfig struct {
	Adapter      string   `json:"adapter"`
	BaseURL      string   `json:"base_url"`
	APIKeyEnv    string   `json:"api_key_env"`
	AuthTokenEnv string   `json:"auth_token_env"`
	Models       []string `json:"models"`
}

type Config struct {
	Listen         string                    `json:"listen"`
	TimeoutSeconds int                       `json:"timeout_seconds"`
	PollIntervalMS int                       `json:"poll_interval_ms"`
	MaxInFlight    int                       `json:"max_in_flight"`
	Providers      map[string]ProviderConfig `json:"providers"`
}

type Route struct {
	Provider Provider
	Token    string
	Models   map[string]bool
}
type Server struct {
	Engine   *Engine
	Routes   map[string]Route
	Slots    chan struct{}
	Requests atomic.Uint64
	Failed   atomic.Uint64
}

func configuredServer(config Config) (*Server, error) {
	ip, _, err := net.SplitHostPort(config.Listen)
	if err != nil || net.ParseIP(ip) == nil || ip == "0.0.0.0" || ip == "::" {
		return nil, fmt.Errorf("监听地址必须是明确的本机或内网 IP")
	}
	parsed := net.ParseIP(ip)
	if !parsed.IsLoopback() && !parsed.IsPrivate() {
		return nil, fmt.Errorf("禁止公网监听")
	}
	if config.TimeoutSeconds < 1 || config.TimeoutSeconds > 270 || config.PollIntervalMS < 100 || config.PollIntervalMS > 30000 || config.MaxInFlight < 1 || config.MaxInFlight > 32 || len(config.Providers) == 0 {
		return nil, fmt.Errorf("超时、轮询间隔、并发或供应商配置无效")
	}
	server := &Server{Engine: &Engine{Timeout: time.Duration(config.TimeoutSeconds) * time.Second, PollInterval: time.Duration(config.PollIntervalMS) * time.Millisecond, DownloadClient: resultClient()}, Routes: map[string]Route{}, Slots: make(chan struct{}, config.MaxInFlight)}
	for name, p := range config.Providers {
		if !regexp.MustCompile(`^[a-z][a-z0-9-]{0,40}$`).MatchString(name) {
			return nil, fmt.Errorf("供应商路由名称无效")
		}
		base := strings.TrimRight(p.BaseURL, "/")
		u, e := url.Parse(base)
		if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("供应商 Base URL 必须为 HTTPS")
		}
		key, token := os.Getenv(p.APIKeyEnv), os.Getenv(p.AuthTokenEnv)
		if key == "" || len(token) < 32 || len(p.Models) == 0 {
			return nil, fmt.Errorf("供应商 %s 缺少凭据或模型列表", name)
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 60 * time.Second
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var provider Provider
		switch p.Adapter {
		case "apimart":
			provider = &APIMart{BaseURL: base, Key: key, Client: client}
		default:
			return nil, fmt.Errorf("供应商 %s 的适配器未实现", name)
		}
		models := map[string]bool{}
		for _, m := range p.Models {
			if m == "" {
				return nil, fmt.Errorf("模型名称为空")
			}
			models[m] = true
		}
		server.Routes[name] = Route{Provider: provider, Token: token, Models: models}
	}
	return server, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		writeJSON(w, 200, map[string]any{"ok": true, "revision": revision, "in_flight": len(s.Slots), "requests": s.Requests.Load(), "failed": s.Failed.Load()})
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if r.Method != "POST" || len(parts) != 4 || parts[1] != "v1" || parts[2] != "images" || (parts[3] != "generations" && parts[3] != "edits") {
		writeError(w, apiError(404, "not_found", "接口不存在"))
		return
	}
	route, ok := s.Routes[parts[0]]
	if !ok || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+route.Token)) != 1 {
		writeError(w, apiError(401, "invalid_api_key", "适配服务凭据无效"))
		return
	}
	select {
	case s.Slots <- struct{}{}:
		defer func() { <-s.Slots }()
	default:
		writeError(w, apiError(429, "adapter_busy", "适配服务繁忙，尚未提交任务"))
		return
	}
	number := s.Requests.Add(1)
	started := time.Now()
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	request, parseErr := parseRequest(r, parts[3] == "edits")
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if parseErr != nil {
		writeError(w, parseErr)
		return
	}
	if !route.Models[request.Model] {
		writeError(w, invalid("本渠道未开放请求的模型"))
		return
	}
	result, apiErr := s.Engine.Generate(r.Context(), route.Provider, request, func(taskID string) {
		log.Printf("request=%d provider=%s task=%s accepted=true", number, parts[0], taskID)
	})
	if apiErr != nil {
		s.Failed.Add(1)
		log.Printf("request=%d provider=%s code=%s elapsed_ms=%d", number, parts[0], apiErr.Code, time.Since(started).Milliseconds())
		writeError(w, apiErr)
		return
	}
	if r.Context().Err() != nil {
		return
	}
	writeJSON(w, 200, result)
	log.Printf("request=%d provider=%s images=%d elapsed_ms=%d", number, parts[0], len(result.Data), time.Since(started).Milliseconds())
}

func parseRequest(r *http.Request, edit bool) (*ImageRequest, *APIError) {
	request := &ImageRequest{Edit: edit}
	if !edit {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, invalid("请求体超过限制或读取失败")
		}
		if err = common.Unmarshal(body, request); err != nil {
			return nil, invalid("图片 JSON 参数无效")
		}
		return request, nil
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return nil, invalid("编辑请使用 multipart 图片文件")
	}
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		return nil, invalid("编辑表单无效或超过大小限制")
	}
	values := map[string]any{}
	for key, list := range r.MultipartForm.Value {
		if len(list) != 1 {
			return nil, invalid("编辑参数不能重复")
		}
		switch key {
		case "n", "stream", "output_compression":
			var v any
			if common.Unmarshal([]byte(list[0]), &v) != nil {
				return nil, invalid("编辑参数类型无效")
			}
			values[key] = v
		default:
			values[key] = list[0]
		}
	}
	raw, err := common.Marshal(values)
	if err != nil || common.Unmarshal(raw, request) != nil {
		return nil, invalid("编辑参数类型无效")
	}
	request.Images = append(request.Images, r.MultipartForm.File["image"]...)
	request.Images = append(request.Images, r.MultipartForm.File["image[]"]...)
	var indices []int
	for key := range r.MultipartForm.File {
		if key == "image" || key == "image[]" || key == "mask" {
			continue
		}
		if !strings.HasPrefix(key, "image[") || !strings.HasSuffix(key, "]") {
			return nil, invalid("存在无法识别的图片文件字段")
		}
		index, e := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(key, "image["), "]"))
		if e != nil || index < 0 || key != fmt.Sprintf("image[%d]", index) {
			return nil, invalid("参考图索引无效")
		}
		indices = append(indices, index)
	}
	sort.Ints(indices)
	for _, index := range indices {
		request.Images = append(request.Images, r.MultipartForm.File[fmt.Sprintf("image[%d]", index)]...)
	}
	masks := r.MultipartForm.File["mask"]
	if len(masks) > 1 {
		return nil, invalid("最多支持一张蒙版")
	}
	if len(masks) == 1 {
		request.Mask = masks[0]
	}
	return request, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := common.Marshal(value)
	if err != nil {
		http.Error(w, "结果编码失败", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func writeError(w http.ResponseWriter, err *APIError) {
	writeJSON(w, err.Status, map[string]any{"error": err})
}

func main() {
	if len(os.Args) != 2 {
		log.Fatal("用法：image-adapter /path/config.json")
	}
	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		log.Fatal("无法读取服务配置")
	}
	var config Config
	if common.Unmarshal(data, &config) != nil {
		log.Fatal("服务配置 JSON 无效")
	}
	handler, err := configuredServer(config)
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{Addr: config.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 60 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 280*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Print("适配服务排空超时")
		}
	}()
	log.Printf("图片适配服务启动 revision=%s listen=%s", revision, config.Listen)
	if err = server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
	<-drained
}
