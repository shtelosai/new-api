package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	_ "golang.org/x/image/webp"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var (
	ErrImageJobConflict = errors.New("同一请求标识不能用于不同图片需求")
	ErrImageJobNotFound = errors.New("图片任务不存在")
	ErrImageJobQuota    = errors.New("图片任务额度不足")
	ErrImageJobAuth     = errors.New("图片任务凭据已失效")
	ErrImageJobLease    = errors.New("图片任务租约已失效")
	imageJobIDPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

var ImageJobSizes = []string{"1:1", "3:2", "2:3", "4:3", "3:4", "5:4", "4:5", "16:9", "9:16", "2:1", "1:2", "21:9", "9:21"}

type ImageJobRequest struct {
	ClientRequestID   string   `json:"client_request_id"`
	ClientSessionID   string   `json:"client_session_id"`
	Prompt            string   `json:"prompt"`
	Size              string   `json:"size"`
	Resolution        string   `json:"resolution"`
	OutputFormat      string   `json:"output_format,omitempty"`
	OutputCompression *int     `json:"output_compression,omitempty"`
	Background        string   `json:"background"`
	Quality           string   `json:"quality,omitempty"`
	ImageURLs         []string `json:"image_urls,omitempty"`
	MaskURL           string   `json:"mask_url,omitempty"`
}

func (r *ImageJobRequest) Normalize() error {
	if !imageJobIDPattern.MatchString(r.ClientRequestID) || !imageJobIDPattern.MatchString(r.ClientSessionID) {
		return errors.New("请求和会话标识必须为 1 至 128 位字母、数字、下划线或连字符")
	}
	if strings.TrimSpace(r.Prompt) == "" || utf8.RuneCountInString(r.Prompt) > 8000 {
		return errors.New("图片描述须为 1 至 8000 字")
	}
	if r.Size == "" {
		r.Size = "1:1"
	}
	if r.Resolution == "" {
		r.Resolution = "1k"
	}
	if r.Background == "" {
		r.Background = "opaque"
	}
	if !stringsIn(ImageJobSizes, r.Size) || !stringsIn([]string{"1k", "2k", "4k"}, r.Resolution) {
		return errors.New("图片比例或分辨率不受支持")
	}
	if !stringsIn([]string{"", "png", "jpeg", "webp"}, r.OutputFormat) || !stringsIn([]string{"opaque", "transparent", "auto"}, r.Background) || !stringsIn([]string{"", "low", "medium", "high", "xhigh", "max"}, r.Quality) {
		return errors.New("图片格式、背景或质量参数无效")
	}
	if r.Background == "transparent" && r.OutputFormat == "jpeg" {
		return errors.New("JPEG 不支持透明背景")
	}
	if r.OutputCompression != nil && (*r.OutputCompression < 0 || *r.OutputCompression > 100 || (r.OutputFormat != "jpeg" && r.OutputFormat != "webp")) {
		return errors.New("压缩参数仅适用于 JPEG/WebP，范围为 0 至 100")
	}
	if len(r.ImageURLs) > 16 || (r.MaskURL != "" && len(r.ImageURLs) == 0) {
		return errors.New("最多 16 张参考图，蒙版须配合参考图")
	}
	refs := append([]string{}, r.ImageURLs...)
	if r.MaskURL != "" {
		refs = append(refs, r.MaskURL)
	}
	total := 0
	for index, ref := range refs {
		total += len(ref)
		if total > 60<<20 {
			return errors.New("参考图编码总大小超过限制")
		}

		if strings.HasPrefix(ref, "data:image/png;base64,") || strings.HasPrefix(ref, "data:image/jpeg;base64,") || strings.HasPrefix(ref, "data:image/webp;base64,") {
			prefix, encoded, _ := strings.Cut(ref, ",")
			limit := 20 << 20
			if index == len(r.ImageURLs) {
				limit = 4 << 20
			}
			if len(encoded) > base64.StdEncoding.EncodedLen(limit) {
				return errors.New("单张参考图超过大小限制")
			}
			data, err := base64.StdEncoding.Strict().DecodeString(encoded)

			if err != nil || len(data) > limit {
				return errors.New("参考图 Base64 无效或超过大小限制")
			}
			cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
			if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 8192 || cfg.Height > 8192 || int64(cfg.Width)*int64(cfg.Height) > 40_000_000 || prefix != "data:image/"+format+";base64" {
				return errors.New("参考图片格式或尺寸无效")
			}
			if _, _, err = image.Decode(bytes.NewReader(data)); err != nil {
				return errors.New("参考图片文件不完整")
			}
			continue
		}
		u, err := url.Parse(ref)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(ref) > 8192 || (u.Port() != "" && u.Port() != "443") {
			return errors.New("参考图必须为 PNG/JPEG/WebP 数据或 HTTPS 地址")
		}
	}
	return nil
}

func stringsIn(values []string, value string) bool {
	for _, v := range values {
		if v == value {
			return true
		}
	}
	return false
}
func (r ImageJobRequest) Family() string    { return "sunburst" }
func (r ImageJobRequest) ModelName() string { return "twork-image-" + r.Family() + "-async" }
func (r ImageJobRequest) Price() (int, int) {
	switch r.Resolution {
	case "1k":
		return 30, 150000
	case "2k":
		return 40, 200000
	case "4k":
		return 80, 400000
	default:
		return 0, 0
	}
}
func (r ImageJobRequest) Hash() (string, error) {
	b, err := common.Marshal(r)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sha256.Sum256(b)), nil
}
func ImageJobID(tokenID int, requestID string) string {
	return fmt.Sprintf("ij_%x", sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", tokenID, requestID))))[:43]
}

// ImageJob 是异步生图金额事实。所有金额变更先锁 token，再锁 job，最后锁付款钱包。
// 请求文件与结果文件位于专用持久卷，不把参考图 base64 放入 MySQL TEXT 列。
type ImageJob struct {
	ID                string `gorm:"primaryKey;size:64"`
	TokenID           int    `gorm:"uniqueIndex:idx_image_job_request,priority:1;index:idx_image_job_token_session,priority:1"`
	UserID            int    `gorm:"index"`
	ClientRequestID   string `gorm:"size:128;uniqueIndex:idx_image_job_request,priority:2"`
	ClientSessionID   string `gorm:"size:128;index:idx_image_job_token_session,priority:2"`
	RequestHash       string `gorm:"size:64"`
	Status            string `gorm:"size:20;index:idx_image_job_work,priority:1"`
	ModelFamily       string `gorm:"size:16"`
	Resolution        string `gorm:"size:4"`
	Size              string `gorm:"size:8"`
	PriceCents        int
	ReservedQuota     int
	ChargedQuota      int
	ChannelID         int
	AttemptedChannels string `gorm:"type:text"`
	ImageProvider     string `gorm:"size:16"`
	AdapterBaseURL    string `gorm:"size:1024"`
	ProviderTaskID    string `gorm:"size:160"`
	ErrorCode         string `gorm:"size:64"`
	ActualSize        string `gorm:"size:32"`
	OutputFormat      string `gorm:"size:8"`
	Bytes             int64
	ResultHash        string `gorm:"size:64"`
	CreatedAt         int64
	UpdatedAt         int64
	SettledAt         int64 `gorm:"index"`
	FilesRemovedAt    int64
	ExpiresAt         int64
	NextPollAt        int64  `gorm:"index:idx_image_job_work,priority:2"`
	LeaseOwner        string `gorm:"size:64"`
	LeaseUntil        int64  `gorm:"index"`
}

type ImageJobView struct {
	ID                  string `json:"id"`
	Status              string `json:"status"`
	ClientRequestID     string `json:"client_request_id"`
	ClientSessionID     string `json:"client_session_id"`
	ModelFamily         string `json:"model_family"`
	UpstreamModelFamily string `json:"upstream_model_family,omitempty"`
	Resolution          string `json:"resolution"`
	Size                string `json:"size"`
	PriceCents          int    `json:"price_cents"`
	CreatedAt           int64  `json:"created_at"`
	UpdatedAt           int64  `json:"updated_at"`
	ExpiresAt           int64  `json:"expires_at"`
	CanDownload         bool   `json:"can_download"`
	ActualSize          string `json:"actual_size,omitempty"`
	OutputFormat        string `json:"output_format,omitempty"`
	Bytes               int64  `json:"bytes,omitempty"`
	ErrorCode           string `json:"error_code,omitempty"`
}

func (j ImageJob) View() ImageJobView {
	// v1 客户端曾按分辨率校验此字段；保留兼容槽位，实际模型单独返回。
	legacyFamily := "sunburst"
	if j.Resolution == "1k" {
		legacyFamily = "flare"
	}
	return ImageJobView{ID: j.ID, Status: j.Status, ClientRequestID: j.ClientRequestID, ClientSessionID: j.ClientSessionID, ModelFamily: legacyFamily, UpstreamModelFamily: j.ModelFamily, Resolution: j.Resolution, Size: j.Size, PriceCents: j.PriceCents, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, ExpiresAt: j.ExpiresAt, CanDownload: j.Status == "succeeded" && j.ExpiresAt > time.Now().Unix(), ActualSize: j.ActualSize, OutputFormat: j.OutputFormat, Bytes: j.Bytes, ErrorCode: j.ErrorCode}
}
func (j ImageJob) Terminal() bool {
	return stringsIn([]string{"succeeded", "failed", "canceled", "expired"}, j.Status)
}

func ImageJobTokenValid(token *Token) bool {
	return (token.Status == common.TokenStatusEnabled || token.Status == common.TokenStatusExhausted) && (token.ExpiredTime == -1 || token.ExpiredTime >= time.Now().Unix())
}
func ImageJobIdentity(ctx context.Context, key string) (*Token, *User, error) {
	var token Token
	if key == "" || DB == nil {
		return nil, nil, ErrImageJobAuth
	}
	if err := DB.WithContext(ctx).Session(&gorm.Session{Logger: DB.Logger.LogMode(logger.Silent)}).Where(map[string]any{"key": key}).First(&token).Error; err != nil {
		return nil, nil, ErrImageJobAuth
	}
	if !ImageJobTokenValid(&token) {
		return nil, nil, ErrImageJobAuth
	}
	var user User
	if err := DB.WithContext(ctx).First(&user, token.UserId).Error; err != nil || user.Status != common.UserStatusEnabled {
		return nil, nil, ErrImageJobAuth
	}
	return &token, &user, nil
}

func ImageJobChannels(ctx context.Context, tokenID int, r ImageJobRequest) ([]*Channel, error) {
	return imageJobChannels(DB.WithContext(ctx), tokenID, r)
}
func imageJobChannels(db *gorm.DB, tokenID int, r ImageJobRequest) ([]*Channel, error) {
	var token Token
	if err := db.First(&token, tokenID).Error; err != nil {
		return nil, err
	}
	if !ImageJobTokenValid(&token) {
		return nil, ErrImageJobAuth
	}
	var user User
	if err := db.First(&user, token.UserId).Error; err != nil {
		return nil, err
	}
	if user.Status != common.UserStatusEnabled {
		return nil, ErrImageJobAuth
	}
	group := token.Group
	if group == "" {
		group = user.Group
	}
	if !imageJobGroupAllowed(user.Group, group) || !ratio_setting.ContainsGroupRatio(group) || group == "auto" {
		return nil, ErrTworkRouteDenied
	}
	models := []string{"twork-image-sunburst-async", "twork-image-flare-async"}
	grants := db.Model(&TokenModelChannel{}).Select("channel_id").Where("token_id = ? AND model_id IN ?", tokenID, models)
	abilities := db.Model(&Ability{}).Select("channel_id").Where(map[string]any{"group": group, "enabled": true}).Where("model IN ?", models)
	var channels []*Channel
	if err := db.Where("id IN (?) AND id IN (?) AND status = ?", grants, abilities, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}
	valid := make([]*Channel, 0, len(channels))
	for _, ch := range channels {
		runtime, err := ch.TworkRuntime()
		if err != nil || runtime != "image_async" {
			continue
		}
		provider, family, err := ch.ImageJobProvider()
		if err != nil {
			continue
		}
		channelModel := "twork-image-" + family + "-async"
		if token.ModelLimitsEnabled && !token.GetModelLimitsMap()[channelModel] {
			continue
		}
		// 候选跨模型时仍逐对核对授权，不能把同一渠道的其他模型权限借过来。
		var granted, enabled int64
		if err := db.Model(&TokenModelChannel{}).Where("token_id = ? AND channel_id = ? AND model_id = ?", tokenID, ch.Id, channelModel).Count(&granted).Error; err != nil {
			return nil, err
		}
		if err := db.Model(&Ability{}).Where(map[string]any{"group": group, "model": channelModel, "channel_id": ch.Id, "enabled": true}).Count(&enabled).Error; err != nil {
			return nil, err
		}
		if granted == 0 || enabled == 0 {
			continue
		}
		if !stringsIn(strings.Split(ch.Models, ","), channelModel) || !stringsIn(strings.Split(ch.Group, ","), group) || ch.BaseURL == nil || *ch.BaseURL == "" || ch.Key == "" || ch.ChannelInfo.IsMultiKey {
			continue
		}
		if provider == "kie" && (r.MaskURL != "" || r.Quality != "" || !stringsIn([]string{"", "png", "jpeg"}, r.OutputFormat) || r.OutputCompression != nil || !ImageJobKieSize(r.Size)) {
			continue
		}
		var disabled int64
		if err := db.Model(&ChannelModelDisabled{}).Where("channel_id = ? AND model IN ?", ch.Id, []string{channelModel, "gpt-image-2.5-" + family}).Count(&disabled).Error; err != nil {
			return nil, err
		}
		if disabled > 0 {
			continue
		}
		valid = append(valid, ch)
	}
	sort.SliceStable(valid, func(i, j int) bool {
		_, familyA, _ := valid[i].ImageJobProvider()
		_, familyB, _ := valid[j].ImageJobProvider()
		if familyA != familyB {
			return familyA == "sunburst"
		}
		a, b := int64(0), int64(0)
		if valid[i].Priority != nil {
			a = *valid[i].Priority
		}
		if valid[j].Priority != nil {
			b = *valid[j].Priority
		}
		if a == b {
			return valid[i].Id < valid[j].Id
		}
		return a > b
	})
	return valid, nil
}

func CreateImageJob(ctx context.Context, tokenID int, r ImageJobRequest) (*ImageJob, bool, error) {
	if err := r.Normalize(); err != nil {
		return nil, false, err
	}
	hash, err := r.Hash()
	if err != nil {
		return nil, false, err
	}
	if err = PrepareImageJobAccounting(ctx, tokenID); err != nil {
		return nil, false, err
	}
	var job ImageJob
	created := false
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var token Token
		if err := lockForUpdate(tx).First(&token, tokenID).Error; err != nil {
			return err
		}
		if !ImageJobTokenValid(&token) {
			return ErrImageJobAuth
		}
		err := lockForUpdate(tx).Where("token_id = ? AND client_request_id = ?", tokenID, r.ClientRequestID).First(&job).Error
		if err == nil {
			if job.RequestHash != hash {
				return ErrImageJobConflict
			}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		channels, err := imageJobChannels(tx, tokenID, r)
		if err != nil {
			return err
		}
		if len(channels) == 0 {
			return ErrTworkRouteDenied
		}
		cents, quota := r.Price()
		if quota <= 0 {
			return ErrImageJobQuota
		}
		var user User
		if err := lockForUpdate(tx).First(&user, token.UserId).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrImageJobAuth
		}
		if (!token.UnlimitedQuota && token.RemainQuota < quota) || user.Quota < quota {
			return ErrImageJobQuota
		}
		if !token.UnlimitedQuota {
			if err := tx.Model(&Token{}).Where("id = ?", token.Id).Update("remain_quota", gorm.Expr("remain_quota - ?", quota)).Error; err != nil {
				return err
			}
		}
		if err := tx.Model(&User{}).Where("id = ?", user.Id).Update("quota", gorm.Expr("quota - ?", quota)).Error; err != nil {
			return err
		}
		now := time.Now().Unix()
		job = ImageJob{ID: ImageJobID(tokenID, r.ClientRequestID), TokenID: tokenID, UserID: user.Id, ClientRequestID: r.ClientRequestID, ClientSessionID: r.ClientSessionID, RequestHash: hash, Status: "queued", ModelFamily: r.Family(), Resolution: r.Resolution, Size: r.Size, PriceCents: cents, ReservedQuota: quota, CreatedAt: now, UpdatedAt: now, ExpiresAt: now + 7*86400, AttemptedChannels: "[]"}
		if err := tx.Create(&job).Error; err != nil {
			return err
		}
		created = true
		return nil
	})
	return &job, created, err
}

func GetImageJob(ctx context.Context, tokenID int, id string) (*ImageJob, error) {
	var job ImageJob
	err := DB.WithContext(ctx).Where("id = ? AND token_id = ?", id, tokenID).First(&job).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrImageJobNotFound
	}
	return &job, err
}
func ListImageJobs(ctx context.Context, tokenID int, sessionID, cursor string) ([]ImageJob, error) {
	q := DB.WithContext(ctx).Where("token_id = ?", tokenID)
	if sessionID != "" {
		q = q.Where("client_session_id = ?", sessionID)
	}
	if cursor != "" {
		previous, err := GetImageJob(ctx, tokenID, cursor)
		if err != nil {
			return nil, err
		}
		q = q.Where("created_at < ? OR (created_at = ? AND id < ?)", previous.CreatedAt, previous.CreatedAt, previous.ID)
	}
	var jobs []ImageJob
	err := q.Order("created_at DESC, id DESC").Limit(101).Find(&jobs).Error
	return jobs, err
}

// ClaimImageJob 使用持久 CAS 租约，多实例/重启不会同时占有同一任务。
func ClaimImageJob(ctx context.Context, owner string, now, seconds int64) (*ImageJob, error) {
	var jobs []ImageJob
	if err := DB.WithContext(ctx).Where("status IN ? AND next_poll_at <= ? AND lease_until <= ?", []string{"queued", "submitting", "running", "downloading", "settling", "unknown"}, now, now).Order("next_poll_at ASC, created_at ASC").Limit(20).Find(&jobs).Error; err != nil {
		return nil, err
	}
	for _, job := range jobs {
		res := DB.WithContext(ctx).Model(&ImageJob{}).Where("id = ? AND lease_until <= ? AND status = ?", job.ID, now, job.Status).Updates(map[string]any{"lease_owner": owner, "lease_until": now + seconds})
		if res.Error != nil {
			return nil, res.Error
		}
		if res.RowsAffected == 1 {
			job.LeaseOwner = owner
			job.LeaseUntil = now + seconds
			return &job, nil
		}
	}
	return nil, nil
}
func UpdateImageJobLease(ctx context.Context, job *ImageJob, fields map[string]any) error {
	fields["updated_at"] = time.Now().Unix()
	res := DB.WithContext(ctx).Model(&ImageJob{}).Where("id = ? AND lease_owner = ? AND lease_until > ? AND status NOT IN ?", job.ID, job.LeaseOwner, time.Now().Unix(), []string{"succeeded", "failed", "canceled", "expired"}).Updates(fields)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrImageJobLease
	}
	return nil
}

func ImageJobKieSize(size string) bool {
	return stringsIn([]string{"1:1", "3:2", "2:3", "4:3", "3:4", "16:9", "9:16", "21:9"}, size)
}

// 直接读取现有分组策略；worker 恢复时也不得跳过已撤销的分组权限。
func imageJobGroupAllowed(userGroup, group string) bool {
	groups := setting.GetUserUsableGroupsCopy()
	if special, ok := ratio_setting.GetGroupRatioSetting().GroupSpecialUsableGroup.Get(userGroup); ok {
		for name, description := range special {
			if strings.HasPrefix(name, "-:") {
				delete(groups, strings.TrimPrefix(name, "-:"))
			} else {
				groups[strings.TrimPrefix(name, "+:")] = description
			}
		}
	}
	if userGroup != "" {
		groups[userGroup] = "用户分组"
	}
	_, ok := groups[group]
	return ok
}
