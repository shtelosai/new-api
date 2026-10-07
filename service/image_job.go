package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	_ "golang.org/x/image/webp"
)

type ImageJobService struct {
	DataDir      string
	Client       *http.Client
	PollInterval time.Duration
	LeaseSeconds int64
	lastCleanup  atomic.Int64
}

var currentImageJobService atomic.Pointer[ImageJobService]
var ErrImageJobCreationDisabled = errors.New("异步图片暂不接受新任务")

func ImageJobCreationEnabled() bool {
	return common.GetEnvOrDefaultBool("TWORK_IMAGE_TASKS_ENABLED", false)
}

func GetImageJobService() *ImageJobService { return currentImageJobService.Load() }

func NewImageJobService(dataDir string, client *http.Client) (*ImageJobService, error) {
	if !model.ImageJobAccountingSupported() {
		return nil, errors.New("当前缓存、批量额度或日志数据库组合尚不支持异步图片事务")
	}
	if !filepath.IsAbs(dataDir) || model.DB == nil || model.LOG_DB == nil {
		return nil, errors.New("异步图片持久目录或数据库不可用")
	}
	for _, table := range []any{&model.ImageJob{}, &model.ImageJobProjection{}} {
		if !model.DB.Migrator().HasTable(table) {
			return nil, errors.New("异步图片主库 DDL 尚未执行")
		}
	}
	if !model.LOG_DB.Migrator().HasTable(&model.ImageJobLogReceipt{}) {
		return nil, errors.New("异步图片日志 DDL 尚未执行")
	}
	for _, dir := range []string{"requests", "results"} {
		if err := os.MkdirAll(filepath.Join(dataDir, dir), 0700); err != nil {
			return nil, errors.New("异步图片持久目录不可写")
		}
	}
	if client == nil {
		client = &http.Client{Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return &ImageJobService{DataDir: dataDir, Client: client, PollInterval: 3 * time.Second, LeaseSeconds: 180}, nil
}

// 新入口默认关闭，启用失败只关闭异步能力，不中断任何旧同步服务。
func StartImageJobWorker() func() {
	if !common.GetEnvOrDefaultBool("TWORK_IMAGE_TASKS_ACCOUNTING_ENABLED", ImageJobCreationEnabled()) {
		return func() {}
	}
	s, err := NewImageJobService(os.Getenv("TWORK_IMAGE_TASKS_DATA_DIR"), nil)
	if err != nil {
		common.SysError("异步图片未启用: " + err.Error())
		return func() {}
	}
	currentImageJobService.Store(s)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(s.PollInterval)
		defer ticker.Stop()
		for {
			if err := s.RunOnce(ctx); err != nil && ctx.Err() == nil {
				common.SysError("异步图片任务推进暂时失败，等待持久任务恢复")
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() { currentImageJobService.Store(nil); cancel(); <-done }
}

func (s *ImageJobService) Create(ctx context.Context, tokenID int, r model.ImageJobRequest, replayOnly ...bool) (*model.ImageJob, bool, error) {
	if !model.ImageJobAccountingSupported() {
		return nil, false, errors.New("异步图片金额同步暂不可用")
	}
	if err := r.Normalize(); err != nil {
		return nil, false, err
	}
	id := model.ImageJobID(tokenID, r.ClientRequestID)
	digest, err := r.Hash()
	if err != nil {
		return nil, false, err
	}
	existing, err := model.GetImageJob(ctx, tokenID, id)
	if err == nil {
		if existing.RequestHash != digest {
			return nil, false, model.ErrImageJobConflict
		}
		return existing, false, nil
	}
	if !errors.Is(err, model.ErrImageJobNotFound) {
		return nil, false, err
	}
	if !ImageJobCreationEnabled() || (len(replayOnly) > 0 && replayOnly[0]) {
		return nil, false, ErrImageJobCreationDisabled
	}
	// 先拒绝无授权/无额度请求，避免在磁盘上积累不可能创建的输入；事务内仍会再次核验。
	channels, err := model.ImageJobChannels(ctx, tokenID, r)
	if err != nil {
		return nil, false, err
	}
	if len(channels) == 0 {
		return nil, false, model.ErrTworkRouteDenied
	}
	if err = model.PrepareImageJobAccounting(ctx, tokenID); err != nil {
		return nil, false, err
	}
	var token model.Token
	if err = model.DB.WithContext(ctx).First(&token, tokenID).Error; err != nil {
		return nil, false, err
	}
	_, quota := r.Price()
	if !token.UnlimitedQuota && token.RemainQuota < quota {
		return nil, false, model.ErrImageJobQuota
	}
	var wallet model.User
	if err = model.DB.WithContext(ctx).First(&wallet, token.UserId).Error; err != nil {
		return nil, false, err
	}
	if wallet.Quota < quota {
		return nil, false, model.ErrImageJobQuota
	}
	encoded, err := common.Marshal(r)
	if err != nil {
		return nil, false, err
	}
	if len(encoded) > 64<<20 {
		return nil, false, errors.New("图片请求超过完整 JSON 大小限制")
	}
	if err = s.writeImmutable("requests", id, encoded); err != nil {
		return nil, false, err
	}
	return model.CreateImageJob(ctx, tokenID, r)
}

// writeImmutable 使用同卷临时文件与原子硬链接，崩溃/并发均不会覆盖同一任务的数据。
func (s *ImageJobService) writeImmutable(kind, id string, data []byte) error {
	destination := filepath.Join(s.DataDir, kind, id)
	old, err := os.ReadFile(destination)
	if err == nil {
		if !bytes.Equal(old, data) {
			return model.ErrImageJobConflict
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(destination), ".image-")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(name, destination); err != nil {
		if !os.IsExist(err) {
			return err
		}
		old, readErr := os.ReadFile(destination)
		if readErr != nil {
			return readErr
		}
		if !bytes.Equal(old, data) {
			return model.ErrImageJobConflict
		}
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *ImageJobService) Result(ctx context.Context, tokenID int, id string) ([]byte, string, error) {
	job, err := model.GetImageJob(ctx, tokenID, id)
	if err != nil {
		return nil, "", err
	}
	if job.Status != "succeeded" {
		return nil, "", errors.New("image_result_not_ready")
	}
	if job.ExpiresAt <= time.Now().Unix() {
		return nil, "", errors.New("image_result_expired")
	}
	b, err := os.ReadFile(filepath.Join(s.DataDir, "results", job.ID))
	if err != nil {
		return nil, "", errors.New("image_result_unavailable")
	}
	if len(b) > 64<<20 || fmt.Sprintf("%x", sha256.Sum256(b)) != job.ResultHash {
		return nil, "", errors.New("image_result_invalid")
	}
	return b, "image/" + job.OutputFormat, nil
}

func ImageJobCapabilities(ctx context.Context, tokenID int) map[string]any {
	capabilities := map[string]any{"async": false, "protocol_version": 1, "prices_cents": map[string]int{"1k": 30, "2k": 40, "4k": 80}, "resolutions": []string{}, "sizes": []string{}, "supports": map[string]bool{"edit": false, "mask": false, "transparent": false}, "max_reference_images": 16}
	if !ImageJobCreationEnabled() || GetImageJobService() == nil || !model.ImageJobAccountingSupported() {
		return capabilities
	}
	resolutions := []string{}
	sizeSet := map[string]bool{}
	mask := false
	for _, res := range []string{"1k", "2k", "4k"} {
		channels, err := model.ImageJobChannels(ctx, tokenID, model.ImageJobRequest{Resolution: res, Size: "1:1", Background: "opaque"})
		if err != nil {
			continue
		}
		if len(channels) == 0 {
			continue
		}
		resolutions = append(resolutions, res)
		for _, ch := range channels {
			provider, _, _ := ch.ImageJobProvider()
			for _, size := range model.ImageJobSizes {
				if provider != "kie" || model.ImageJobKieSize(size) {
					sizeSet[size] = true
				}
			}
			if provider == "apimart" {
				mask = true
			}
		}
	}
	sizes := []string{}
	for _, size := range model.ImageJobSizes {
		if sizeSet[size] {
			sizes = append(sizes, size)
		}
	}
	capabilities["async"] = len(resolutions) > 0
	capabilities["resolutions"] = resolutions
	capabilities["sizes"] = sizes
	capabilities["supports"] = map[string]bool{"edit": len(resolutions) > 0, "mask": mask, "transparent": len(resolutions) > 0}
	return capabilities
}

// ValidateImageJobResult 必须完整解码图片；APIMart 原生档位按官方像素表精确核验。
func ValidateImageJobResult(data []byte, r model.ImageJobRequest, provider string) (string, string, error) {
	if len(data) == 0 || len(data) > 64<<20 {
		return "", "", errors.New("image_result_invalid")
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width < 64 || cfg.Height < 64 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 || (format != "png" && format != "jpeg" && format != "webp") {
		return "", "", errors.New("image_result_invalid")
	}
	picture, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return "", "", errors.New("image_result_invalid")
	}
	if r.OutputFormat != "" && r.OutputFormat != format {
		return "", "", errors.New("image_output_format_mismatch")
	}
	if r.Background == "transparent" {
		opaque, ok := picture.(interface{ Opaque() bool })
		if !ok || opaque.Opaque() {
			return "", "", errors.New("image_transparency_mismatch")
		}
	}
	actual := fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	if provider == "apimart" {
		expected, ok := imageJobNativeSize(r.Size, r.Resolution)
		if !ok || expected != actual {
			return "", "", errors.New("image_resolution_mismatch")
		}
	}
	if provider == "kie" {
		parts := strings.Split(r.Size, ":")
		if len(parts) != 2 {
			return "", "", errors.New("image_resolution_mismatch")
		}
		a, e1 := strconv.Atoi(parts[0])
		b, e2 := strconv.Atoi(parts[1])
		if e1 != nil || e2 != nil || a <= 0 || b <= 0 {
			return "", "", errors.New("image_resolution_mismatch")
		}
		ratio := float64(cfg.Width) / float64(cfg.Height)
		expectedRatio := float64(a) / float64(b)
		if math.Abs(ratio/expectedRatio-1) > 0.01 {
			return "", "", errors.New("image_resolution_mismatch")
		}
		edge := cfg.Width
		if cfg.Height > edge {
			edge = cfg.Height
		}
		area := int64(cfg.Width) * int64(cfg.Height)
		// Kie 未公布固定像素表：这里只拒绝明显低于所购档位的结果，不伪称精确尺寸。
		if (r.Resolution == "1k" && (area < 655360 || edge < 1024)) || (r.Resolution == "2k" && edge < 2048) || (r.Resolution == "4k" && (area < 4900000 || edge < 2880)) {
			return "", "", errors.New("image_resolution_mismatch")
		}
	}
	return actual, format, nil
}

func imageJobNativeSize(ratio, resolution string) (string, bool) {
	sizes := map[string][3]string{
		"1:1": {"1024x1024", "2048x2048", "2880x2880"}, "3:2": {"1536x1024", "2048x1360", "3520x2336"}, "2:3": {"1024x1536", "1360x2048", "2336x3520"},
		"4:3": {"1024x768", "2048x1536", "3312x2480"}, "3:4": {"768x1024", "1536x2048", "2480x3312"}, "5:4": {"1280x1024", "2560x2048", "3216x2576"}, "4:5": {"1024x1280", "2048x2560", "2576x3216"},
		"16:9": {"1536x864", "2048x1152", "3840x2160"}, "9:16": {"864x1536", "1152x2048", "2160x3840"}, "2:1": {"2048x1024", "2688x1344", "3840x1920"}, "1:2": {"1024x2048", "1344x2688", "1920x3840"},
		"21:9": {"2016x864", "2688x1152", "3840x1648"}, "9:21": {"864x2016", "1152x2688", "1648x3840"},
	}
	index, ok := map[string]int{"1k": 0, "2k": 1, "4k": 2}[resolution]
	values, known := sizes[ratio]
	return values[index], ok && known
}

func (s *ImageJobService) adapterRequest(ctx context.Context, ch *model.Channel, endpoint, method string, payload []byte, limit int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, 0, errors.New("image_adapter_unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+ch.Key)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := s.Client.Do(req)
	if err != nil {
		return nil, 0, errors.New("image_adapter_unavailable")
	}
	defer response.Body.Close()
	b, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil || int64(len(b)) > limit {
		return nil, response.StatusCode, errors.New("image_adapter_invalid_response")
	}
	return b, response.StatusCode, nil
}

func imageJobSafeError(code string) string {
	allowed := []string{"image_result_invalid", "image_resolution_mismatch", "image_output_format_mismatch", "image_transparency_mismatch", "image_result_expired", "image_task_timeout", "provider_rejected", "image_submission_unknown", "image_auth_revoked", "image_no_channel", "image_request_invalid"}
	for _, candidate := range allowed {
		if code == candidate {
			return code
		}
	}
	return "image_provider_failed"
}

func imageJobEndpoint(job *model.ImageJob, ch *model.Channel) string {
	base := job.AdapterBaseURL
	if base == "" && ch.BaseURL != nil {
		base = *ch.BaseURL
	}
	return strings.TrimRight(base, "/") + "/v1/image-tasks"
}

func (s *ImageJobService) cleanup(ctx context.Context) error {
	now := time.Now().Unix()
	if previous := s.lastCleanup.Load(); previous > now-3600 || !s.lastCleanup.CompareAndSwap(previous, now) {
		return nil
	}
	var jobs []model.ImageJob
	if err := model.DB.WithContext(ctx).Where("expires_at <= ? AND files_removed_at = ? AND status IN ?", now, 0, []string{"succeeded", "failed", "canceled", "expired"}).Limit(100).Find(&jobs).Error; err != nil {
		return err
	}
	for _, job := range jobs {
		for _, kind := range []string{"requests", "results"} {
			if err := os.Remove(filepath.Join(s.DataDir, kind, job.ID)); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
		if err := model.DB.WithContext(ctx).Model(&model.ImageJob{}).Where("id = ?", job.ID).Update("files_removed_at", now).Error; err != nil {
			return err
		}
	}
	return nil
}
