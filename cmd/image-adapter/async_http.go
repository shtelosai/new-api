package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

func writeAsyncError(w http.ResponseWriter, e *AsyncError) {
	writeJSON(w, e.Status, map[string]any{"error": e})
}
func (s *Server) serveAsync(w http.ResponseWriter, r *http.Request, parts []string) {
	if s.Async == nil {
		writeAsyncError(w, asyncError(404, "not_found", "异步接口未启用", false))
		return
	}
	route, ok := s.Routes[parts[0]]
	if len(parts) == 4 && parts[3] == "callback" && r.Method == http.MethodPost {
		s.serveAsyncCallback(w, r, parts[0], route)
		return
	}
	if !ok || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+route.Token)) != 1 {
		writeAsyncError(w, asyncError(401, "invalid_api_key", "适配服务凭据无效", false))
		return
	}
	if route.AsyncProvider == nil {
		writeAsyncError(w, asyncError(404, "not_found", "该路由未启用异步接口", false))
		return
	}
	if r.Method == http.MethodPost && len(parts) == 3 {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeAsyncError(w, asyncError(400, "invalid_request_error", "请求体超过限制", false))
			return
		}
		// n、回调地址和其他未声明参数必须拒绝，不能静默丢弃用户要求。
		var fields map[string]any
		if common.Unmarshal(body, &fields) != nil || fields == nil {
			writeAsyncError(w, asyncError(400, "invalid_request_error", "图片 JSON 参数无效", false))
			return
		}
		allowed := map[string]bool{"job_id": true, "model": true, "prompt": true, "size": true, "resolution": true, "output_format": true, "output_compression": true, "background": true, "quality": true, "image_urls": true, "mask_url": true}
		for key := range fields {
			if !allowed[key] {
				writeAsyncError(w, asyncError(400, "invalid_request_error", "存在不支持的图片参数", false))
				return
			}
		}
		var request AsyncRequest
		if common.Unmarshal(body, &request) != nil {
			writeAsyncError(w, asyncError(400, "invalid_request_error", "图片参数类型无效", false))
			return
		}
		task, e := s.Async.create(parts[0], &request)
		if e != nil {
			writeAsyncError(w, e)
			return
		}
		writeJSON(w, http.StatusAccepted, task.view())
		return
	}
	if r.Method != http.MethodGet || (len(parts) != 4 && len(parts) != 5) || !asyncIDPattern.MatchString(parts[3]) || len(parts) == 5 && parts[4] != "result" {
		writeAsyncError(w, asyncError(404, "not_found", "接口不存在", false))
		return
	}
	task, err := s.Async.get(parts[0], parts[3])
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			writeAsyncError(w, asyncError(404, "image_task_not_found", "任务不存在", false))
		} else {
			writeAsyncError(w, asyncError(503, "image_task_store_unavailable", "任务状态暂不可用", false))
		}
		return
	}
	if len(parts) == 4 {
		writeJSON(w, 200, task.view())
		return
	}
	if task.Expired {
		writeAsyncError(w, asyncError(410, "image_result_expired", "图片结果已过期，不会重新生成", false))
		return
	}
	if task.Status != "succeeded" {
		writeAsyncError(w, asyncError(409, "image_result_not_ready", "图片尚未完成", false))
		return
	}
	data, err := os.ReadFile(filepath.Join(s.Async.config.DataDir, "results", task.Key))
	if err != nil || len(data) > 64<<20 {
		writeAsyncError(w, asyncError(503, "image_result_unavailable", "图片文件暂不可用，不会重新生成", false))
		return
	}
	digest := sha256.Sum256(data)
	if fmt.Sprintf("%x", digest) != task.ResultHash {
		writeAsyncError(w, asyncError(503, "image_result_invalid", "图片校验失败，不会重新生成", false))
		return
	}
	w.Header().Set("Content-Type", "image/"+task.OutputFormat)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(200)
	_, _ = w.Write(data)
}

func (s *Server) serveAsyncCallback(w http.ResponseWriter, r *http.Request, routeName string, route Route) {
	provider, ok := route.AsyncProvider.(*Kie)
	if !ok || provider.WebhookKey == "" || provider.CallbackURL == "" {
		writeAsyncError(w, asyncError(404, "not_found", "回调未启用", false))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeAsyncError(w, asyncError(400, "invalid_callback", "回调内容无效", false))
		return
	}
	id := gjson.GetBytes(body, "data.taskId").String()
	timestamp := r.Header.Get("X-Webhook-Timestamp")
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	now := time.Now().Unix()
	if !providerIDPattern.MatchString(id) || err != nil || seconds < now-300 || seconds > now+300 {
		writeAsyncError(w, asyncError(401, "invalid_callback_signature", "回调签名无效或过期", false))
		return
	}
	signature, err := base64.StdEncoding.DecodeString(r.Header.Get("X-Webhook-Signature"))
	mac := hmac.New(sha256.New, []byte(provider.WebhookKey))
	_, _ = mac.Write([]byte(id + "." + timestamp))
	if err != nil || !hmac.Equal(signature, mac.Sum(nil)) {
		writeAsyncError(w, asyncError(401, "invalid_callback_signature", "回调签名无效或过期", false))
		return
	}
	// 签名不覆盖 resultJson，故绝不采信回调内容；重复通知只安排同 ID 状态复核。
	result := s.Async.db.Model(&asyncTask{}).Where("route = ? AND provider_task_id = ? AND status IN ?", routeName, id, []string{"running", "unknown"}).Update("updated_at", time.Now().Add(-time.Minute).Unix())
	if result.Error != nil {
		writeAsyncError(w, asyncError(503, "image_task_store_unavailable", "任务状态暂不可用", false))
		return
	}
	writeJSON(w, 200, map[string]bool{"received": true})
}

func asyncEndpoint(parts []string) bool {
	return len(parts) >= 3 && len(parts) <= 5 && parts[1] == "v1" && parts[2] == "image-tasks"
}
