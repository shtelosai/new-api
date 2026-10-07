package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type AsyncConfig struct {
	Enabled            bool   `json:"enabled"`
	DataDir            string `json:"data_dir"`
	Workers            int    `json:"workers"`
	MaxPending         int    `json:"max_pending"`
	TaskTimeoutSeconds int    `json:"task_timeout_seconds"`
	RetentionHours     int    `json:"retention_hours"`
}

type asyncTask struct {
	Key            string `gorm:"primaryKey;size:64"`
	JobID          string `gorm:"size:128"`
	Route          string `gorm:"size:41;index"`
	RequestHash    string `gorm:"size:64"`
	Payload        []byte
	Status         string `gorm:"size:20;index"`
	ProviderTaskID string `gorm:"size:160;index"`
	ErrorCode      string `gorm:"size:64"`
	Retryable      bool
	ActualSize     string `gorm:"size:32"`
	OutputFormat   string `gorm:"size:8"`
	ImageCount     int
	ResultURLs     []byte
	ResultHash     string `gorm:"size:64"`
	CreatedAt      int64
	UpdatedAt      int64
	DeadlineAt     int64
	Expired        bool
}

type asyncStatus struct {
	ID             string `json:"id"`
	Status         string `json:"status"`
	ErrorCode      string `json:"error_code,omitempty"`
	Retryable      bool   `json:"retryable"`
	ActualSize     string `json:"actual_size,omitempty"`
	OutputFormat   string `json:"output_format,omitempty"`
	ImageCount     int    `json:"image_count,omitempty"`
	ProviderTaskID string `json:"provider_task_id,omitempty"`
	ResultExpired  bool   `json:"result_expired,omitempty"`
}

func (t *asyncTask) view() asyncStatus {
	return asyncStatus{ID: t.JobID, Status: t.Status, ErrorCode: t.ErrorCode, Retryable: t.Retryable, ActualSize: t.ActualSize, OutputFormat: t.OutputFormat, ImageCount: t.ImageCount, ProviderTaskID: t.ProviderTaskID, ResultExpired: t.Expired}
}

// AsyncManager 使用独立 SQLite 与进程文件锁，不占用旧同步请求的并发配额。
// 已受理任务的生命周期由服务上下文负责，与创建请求断开无关。
type AsyncManager struct {
	db          *gorm.DB
	config      AsyncConfig
	routes      map[string]Route
	client      *http.Client
	interval    time.Duration
	lock        *os.File
	ctx         context.Context
	cancel      context.CancelFunc
	loop        sync.WaitGroup
	process     sync.Mutex
	start       sync.Once
	close       sync.Once
	closeErr    error
	lastCleanup time.Time
}

func openAsyncManager(config AsyncConfig, routes map[string]Route, client *http.Client, interval time.Duration) (*AsyncManager, error) {
	if config.DataDir == "" || !filepath.IsAbs(config.DataDir) || config.Workers < 1 || config.Workers > 8 || config.MaxPending < config.Workers || config.MaxPending > 1000 || config.TaskTimeoutSeconds < 60 || config.TaskTimeoutSeconds > 86400 || config.RetentionHours < 24 || config.RetentionHours > 720 || interval <= 0 {
		return nil, fmt.Errorf("异步任务目录、并发、容量或保留时间无效")
	}
	if err := os.MkdirAll(config.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("异步任务目录不可写")
	}
	info, err := os.Stat(config.DataDir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("异步任务目录必须仅当前用户可访问（权限 0700）")
	}
	if err := os.MkdirAll(filepath.Join(config.DataDir, "results"), 0700); err != nil {
		return nil, fmt.Errorf("异步结果目录不可写")
	}
	lock, err := os.OpenFile(filepath.Join(config.DataDir, "process.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("异步任务锁无法打开")
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("异步任务目录正由另一进程使用")
	}
	dbPath := filepath.Join(config.DataDir, "tasks.sqlite")
	db, err := gorm.Open(sqlite.Open(dbPath+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		lock.Close()
		return nil, fmt.Errorf("异步任务数据库无法打开")
	}
	sqlDB, err := db.DB()
	if err != nil {
		lock.Close()
		return nil, err
	}
	sqlDB.SetMaxOpenConns(1)
	if err = db.AutoMigrate(&asyncTask{}); err != nil {
		sqlDB.Close()
		lock.Close()
		return nil, fmt.Errorf("异步任务数据库初始化失败")
	}
	if err = os.Chmod(dbPath, 0600); err != nil {
		sqlDB.Close()
		lock.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &AsyncManager{db: db, config: config, routes: routes, client: client, interval: interval, lock: lock, ctx: ctx, cancel: cancel}, nil
}

func (m *AsyncManager) Start() {
	m.start.Do(func() {
		m.loop.Add(1)
		go func() {
			defer m.loop.Done()
			ticker := time.NewTicker(m.interval)
			defer ticker.Stop()
			for {
				if m.ctx.Err() != nil {
					return
				}
				if err := m.runOnce(m.ctx); err != nil && m.ctx.Err() == nil {
					log.Print("异步图片任务存储处理失败，将保留任务并重试查询")
				}
				select {
				case <-m.ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}
func (m *AsyncManager) Close() error {
	m.close.Do(func() {
		m.cancel()
		m.loop.Wait()
		m.process.Lock()
		defer m.process.Unlock()
		db, err := m.db.DB()
		if err == nil {
			err = db.Close()
		}
		m.closeErr = err
		_ = syscall.Flock(int(m.lock.Fd()), syscall.LOCK_UN)
		_ = m.lock.Close()
	})
	return m.closeErr
}

func asyncTaskKey(route, id string) string {
	sum := sha256.Sum256([]byte(route + "\x00" + id))
	return fmt.Sprintf("%x", sum)
}
func (m *AsyncManager) get(route, id string) (*asyncTask, error) {
	var task asyncTask
	err := m.db.First(&task, "key = ? AND route = ?", asyncTaskKey(route, id), route).Error
	return &task, err
}
func (m *AsyncManager) create(route string, request *AsyncRequest) (*asyncTask, *AsyncError) {
	provider := m.routes[route].AsyncProvider
	if provider == nil {
		return nil, asyncError(404, "not_found", "该路由未启用异步生图", false)
	}
	if e := provider.ValidateTask(request); e != nil {
		return nil, e
	}
	if !m.routes[route].Models[request.Model] {
		return nil, asyncError(400, "invalid_request_error", "本渠道未开放请求的模型", false)
	}
	raw, err := common.Marshal(request)
	if err != nil {
		return nil, asyncError(400, "invalid_request_error", "图片参数无效", false)
	}
	hash := sha256.Sum256(raw)
	digest := fmt.Sprintf("%x", hash)
	var task asyncTask
	var apiErr *AsyncError
	err = m.db.Transaction(func(tx *gorm.DB) error {
		find := tx.First(&task, "key = ?", asyncTaskKey(route, request.JobID)).Error
		if find == nil {
			if task.RequestHash != digest {
				apiErr = asyncError(409, "image_task_conflict", "任务 ID 已用于其他参数", false)
			}
			return nil
		}
		if !errors.Is(find, gorm.ErrRecordNotFound) {
			return find
		}
		var pending int64
		if err := tx.Model(&asyncTask{}).Where("status IN ? OR (status = ? AND provider_task_id <> ? AND deadline_at > ?)", []string{"queued", "submitting", "running", "downloading"}, "unknown", "", time.Now().Unix()).Count(&pending).Error; err != nil {
			return err
		}
		if pending >= int64(m.config.MaxPending) {
			apiErr = asyncError(429, "adapter_busy", "异步队列已满，尚未接收任务", true)
			return nil
		}
		now := time.Now().Unix()
		task = asyncTask{Key: asyncTaskKey(route, request.JobID), JobID: request.JobID, Route: route, RequestHash: digest, Payload: raw, Status: "queued", CreatedAt: now, UpdatedAt: now, DeadlineAt: now + int64(m.config.TaskTimeoutSeconds)}
		return tx.Create(&task).Error
	})
	if err != nil {
		return nil, asyncError(503, "image_task_store_unavailable", "任务保存状态暂不可确认，请使用相同 ID 查询或重试", false)
	}
	if apiErr != nil {
		return nil, apiErr
	}
	return &task, nil
}

func (m *AsyncManager) runOnce(ctx context.Context) error {
	m.process.Lock()
	defer m.process.Unlock()
	if time.Since(m.lastCleanup) >= time.Hour {
		if err := m.cleanup(); err != nil {
			return err
		}
		m.lastCleanup = time.Now()
	}
	var tasks []asyncTask
	if err := m.db.Where("expired = ? AND (status IN ? OR (status = ? AND provider_task_id <> ? AND deadline_at > ?))", false, []string{"queued", "submitting", "running", "downloading"}, "unknown", "", time.Now().Unix()).Order("updated_at ASC, key ASC").Limit(m.config.Workers).Find(&tasks).Error; err != nil {
		return err
	}
	var wg sync.WaitGroup
	errorsOut := make(chan error, len(tasks))
	for i := range tasks {
		wg.Add(1)
		go func(task asyncTask) {
			defer wg.Done()
			if err := m.advance(ctx, &task); err != nil {
				errorsOut <- err
			}
		}(tasks[i])
	}
	wg.Wait()
	close(errorsOut)
	for err := range errorsOut {
		return err
	}
	return nil
}

func (m *AsyncManager) save(task *asyncTask) error {
	task.UpdatedAt = time.Now().Unix()
	return m.db.Save(task).Error
}
func (m *AsyncManager) fail(task *asyncTask, code string, unknown, retryable bool) error {
	task.Status = "failed"
	if unknown {
		task.Status = "unknown"
	}
	task.ErrorCode = code
	task.Retryable = retryable && task.ProviderTaskID == ""
	return m.save(task)
}

func (m *AsyncManager) advance(ctx context.Context, task *asyncTask) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	route, ok := m.routes[task.Route]
	if !ok || route.AsyncProvider == nil {
		return m.fail(task, "image_route_unavailable", task.Status != "queued", task.Status == "queued")
	}
	if time.Now().Unix() >= task.DeadlineAt {
		return m.fail(task, "image_task_timeout", task.Status != "queued", task.Status == "queued")
	}
	switch task.Status {
	case "queued":
		var request AsyncRequest
		if err := common.Unmarshal(task.Payload, &request); err != nil {
			return m.fail(task, "image_request_invalid", false, false)
		}
		if !route.Models[request.Model] {
			return m.fail(task, "image_model_unavailable", false, true)
		}
		opCtx, cancel := context.WithTimeout(ctx, 180*time.Second)
		defer cancel()
		if e := prepareAsyncInputs(opCtx, m.client, &request); e != nil {
			return m.fail(task, e.Code, false, e.Retryable)
		}
		// 先持久记录提交意图。这里到保存上游 ID 之间崩溃，重启后只能标记 unknown。
		task.Status = "submitting"
		if err := m.save(task); err != nil {
			return err
		}
		id, e := route.AsyncProvider.SubmitTask(opCtx, &request)
		if e != nil {
			return m.fail(task, e.Code, e.Unknown, e.Retryable)
		}
		if !providerIDPattern.MatchString(id) {
			return m.fail(task, "image_submission_unknown", true, false)
		}
		task.ProviderTaskID = id
		task.Status = "running"
		task.ErrorCode = ""
		task.Retryable = false
		return m.save(task)
	case "submitting":
		if task.ProviderTaskID == "" {
			return m.fail(task, "image_submission_unknown", true, false)
		}
		task.Status = "running"
		return m.save(task)
	case "running", "unknown":
		if task.ProviderTaskID == "" {
			return m.fail(task, "image_status_unknown", true, false)
		}
		pollCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		result, e := route.AsyncProvider.PollTask(pollCtx, task.ProviderTaskID)
		if e != nil {
			if e.Transient {
				return m.save(task)
			}
			return m.fail(task, e.Code, e.Unknown, false)
		}
		if result == nil {
			return m.fail(task, "image_status_unknown", true, false)
		}
		if result.Pending {
			task.Status = "running"
			task.ErrorCode = ""
			return m.save(task)
		}
		if len(result.URLs) != 1 || !validPublicImageURL(result.URLs[0]) {
			return m.fail(task, "image_result_invalid", false, false)
		}
		raw, err := common.Marshal(result.URLs)
		if err != nil {
			return err
		}
		task.ResultURLs = raw
		task.Status = "downloading"
		task.ErrorCode = ""
		return m.save(task)
	case "downloading":
		var urls []string
		if common.Unmarshal(task.ResultURLs, &urls) != nil || len(urls) != 1 || !validPublicImageURL(urls[0]) {
			return m.fail(task, "image_result_invalid", false, false)
		}
		downloadCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		status, picture, err := boundedRequest(downloadCtx, m.client, "GET", urls[0], "", "", nil, 64<<20)
		if err != nil || status != 200 {
			task.Status = "running"
			task.ErrorCode = "image_result_download_pending"
			return m.save(task)
		}
		config, format, err := image.DecodeConfig(bytes.NewReader(picture))
		if err != nil || !validImageDimensions(config) || (format != "png" && format != "jpeg" && format != "webp") {
			return m.fail(task, "image_result_invalid", false, false)
		}
		decoded, _, err := image.Decode(bytes.NewReader(picture))
		if err != nil {
			return m.fail(task, "image_result_invalid", false, false)
		}
		var request AsyncRequest
		if common.Unmarshal(task.Payload, &request) != nil {
			return m.fail(task, "image_request_invalid", false, false)
		}
		if request.Background == "transparent" {
			if opaque, ok := decoded.(interface{ Opaque() bool }); !ok || opaque.Opaque() {
				return m.fail(task, "image_transparency_mismatch", false, false)
			}
		}
		if request.OutputFormat != "" && request.OutputFormat != format {
			return m.fail(task, "image_output_format_mismatch", false, false)
		}
		if expected, ok := apimartNativeSize(request.Size, request.Resolution); ok {
			if _, isAPIMart := route.AsyncProvider.(*APIMart); isAPIMart && expected != fmt.Sprintf("%dx%d", config.Width, config.Height) {
				return m.fail(task, "image_resolution_mismatch", false, false)
			}
		}
		if _, isKie := route.AsyncProvider.(*Kie); isKie && !kieNativeSizeMatches(request.Size, request.Resolution, config.Width, config.Height) {
			return m.fail(task, "image_resolution_mismatch", false, false)
		}
		if err = m.writeResult(task.Key, picture); err != nil {
			return err
		}
		digest := sha256.Sum256(picture)
		task.ResultHash = fmt.Sprintf("%x", digest)
		task.ActualSize = fmt.Sprintf("%dx%d", config.Width, config.Height)
		task.OutputFormat = format
		task.ImageCount = 1
		task.Status = "succeeded"
		task.ErrorCode = ""
		task.Retryable = false
		return m.save(task)
	}
	return nil
}

func (m *AsyncManager) writeResult(key string, data []byte) error {
	dir := filepath.Join(m.config.DataDir, "results")
	file, err := os.CreateTemp(dir, ".image-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, filepath.Join(dir, key)); err != nil {
		return err
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func (m *AsyncManager) cleanup() error {
	var tasks []asyncTask
	cutoff := time.Now().Add(-time.Duration(m.config.RetentionHours) * time.Hour).Unix()
	if err := m.db.Where("expired = ? AND updated_at < ? AND status IN ?", false, cutoff, []string{"succeeded", "failed"}).Limit(100).Find(&tasks).Error; err != nil {
		return err
	}
	for _, task := range tasks {
		if err := os.Remove(filepath.Join(m.config.DataDir, "results", task.Key)); err != nil && !os.IsNotExist(err) {
			return err
		}
		// 保留哈希和终态墓碑，过期 ID 也绝不生成第二次；敏感输入与临时上游地址清空。
		if err := m.db.Model(&asyncTask{}).Where("key = ?", task.Key).Updates(map[string]any{"expired": true, "payload": nil, "result_urls": nil}).Error; err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(filepath.Join(m.config.DataDir, "results"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".image-") || entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.ModTime().Unix() < cutoff {
			if err := os.Remove(filepath.Join(m.config.DataDir, "results", entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

// APIMart 官方原生档位表；4K 不是统一长边 4096，不对图片插值或推测降档。
func apimartNativeSize(ratio, resolution string) (string, bool) {
	sizes := map[string][3]string{
		"1:1": {"1024x1024", "2048x2048", "2880x2880"}, "3:2": {"1536x1024", "2048x1360", "3520x2336"}, "2:3": {"1024x1536", "1360x2048", "2336x3520"},
		"4:3": {"1024x768", "2048x1536", "3312x2480"}, "3:4": {"768x1024", "1536x2048", "2480x3312"}, "5:4": {"1280x1024", "2560x2048", "3216x2576"}, "4:5": {"1024x1280", "2048x2560", "2576x3216"},
		"16:9": {"1536x864", "2048x1152", "3840x2160"}, "9:16": {"864x1536", "1152x2048", "2160x3840"}, "2:1": {"2048x1024", "2688x1344", "3840x1920"}, "1:2": {"1024x2048", "1344x2688", "1920x3840"},
		"21:9": {"2016x864", "2688x1152", "3840x1648"}, "9:21": {"864x2016", "1152x2688", "1648x3840"}, "3:1": {"1536x512", "3072x1024", "3840x1280"}, "1:3": {"512x1536", "1024x3072", "1280x3840"},
	}
	index, ok := map[string]int{"1k": 0, "2k": 1, "4k": 2}[resolution]
	values, known := sizes[ratio]
	return values[index], ok && known
}
