package model

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/tidwall/gjson"
	"gorm.io/gorm"

	"github.com/QuantumNous/new-api/common"
)

// 画布图片模型的价格和能力都放在渠道设置里，运营可改，不需要发版。
const (
	ImageModelProvider      = "openai_images"
	imageModelChannelPrefix = "twork-canvas-"
	imageQuotaPerCent       = 5000 // 500000 quota = 1 元
)

type ImageModelConfig struct {
	DisplayName   string         `json:"display_name"`
	PricesCents   map[string]int `json:"prices_cents"`
	Sizes         []string       `json:"sizes"`
	Edit          bool           `json:"edit"`
	Mask          bool           `json:"mask"`
	MaxReferences int            `json:"max_references"`
	SortOrder     int            `json:"sort_order"`
}

type ImageModelView struct {
	ID            string         `json:"id"`
	Name          string         `json:"name"`
	PricesCents   map[string]int `json:"prices_cents"`
	Resolutions   []string       `json:"resolutions"`
	Sizes         []string       `json:"sizes"`
	Edit          bool           `json:"edit"`
	Mask          bool           `json:"mask"`
	MaxReferences int            `json:"max_references"`
}

// ImageCanvasJobView 是画布任务的公开字段，不含渠道和上游信息。
type ImageCanvasJobView struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	ClientRequestID string `json:"client_request_id"`
	Model           string `json:"model"`
	Resolution      string `json:"resolution"`
	Size            string `json:"size"`
	PriceCents      int    `json:"price_cents"`
	CreatedAt       int64  `json:"created_at"`
	UpdatedAt       int64  `json:"updated_at"`
	ExpiresAt       int64  `json:"expires_at"`
	CanDownload     bool   `json:"can_download"`
	ActualSize      string `json:"actual_size,omitempty"`
	OutputFormat    string `json:"output_format,omitempty"`
	Bytes           int64  `json:"bytes,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
}

func (j ImageJob) CanvasView() ImageCanvasJobView {
	return ImageCanvasJobView{ID: j.ID, Status: j.Status, ClientRequestID: j.ClientRequestID, Model: j.ImageModel, Resolution: j.Resolution, Size: j.Size, PriceCents: j.PriceCents, CreatedAt: j.CreatedAt, UpdatedAt: j.UpdatedAt, ExpiresAt: j.ExpiresAt, CanDownload: j.Status == "succeeded" && j.ExpiresAt > time.Now().Unix(), ActualSize: j.ActualSize, OutputFormat: j.OutputFormat, Bytes: j.Bytes, ErrorCode: j.ErrorCode}
}

// expectedQuota 让旧任务继续走固定分档，画布任务以创建时锁定的价格为准。
func (j ImageJob) expectedQuota() int {
	if j.ImageModel != "" {
		return j.PriceCents * imageQuotaPerCent
	}
	_, quota := (ImageJobRequest{Resolution: j.Resolution}).Price()
	return quota
}

func (j ImageJob) logModelName() string {
	if j.ImageModel != "" {
		return j.ImageModel
	}
	return "twork-image-" + j.ModelFamily + "-async"
}

func imageModelChannelName(id string) string { return imageModelChannelPrefix + id }

func (channel *Channel) ImageModelConfig() (ImageModelConfig, error) {
	var config ImageModelConfig
	if channel.Setting == nil {
		return config, errors.New("画布图片设置缺失")
	}
	value := gjson.Get(*channel.Setting, "twork_image_model")
	if !value.IsObject() || common.UnmarshalJsonStr(value.Raw, &config) != nil {
		return config, errors.New("画布图片设置无效")
	}
	if config.DisplayName == "" || len(config.PricesCents) == 0 || len(config.Sizes) == 0 {
		return config, errors.New("画布图片设置不完整")
	}
	for resolution, cents := range config.PricesCents {
		if !stringsIn([]string{"1k", "2k", "4k"}, resolution) || cents <= 0 {
			return config, errors.New("画布图片价格无效")
		}
	}
	for _, size := range config.Sizes {
		if !stringsIn(ImageJobSizes, size) {
			return config, errors.New("画布图片比例无效")
		}
	}
	return config, nil
}

// ImageModelPrice 返回分和 quota；档位未开放时为 0，由调用方拒绝。
func (channel *Channel) ImageModelPrice(resolution string) (int, int) {
	config, err := channel.ImageModelConfig()
	if err != nil {
		return 0, 0
	}
	cents := config.PricesCents[resolution]
	return cents, cents * imageQuotaPerCent
}

func (config ImageModelConfig) supports(r ImageJobRequest) bool {
	if config.PricesCents[r.Resolution] <= 0 || !stringsIn(config.Sizes, r.Size) {
		return false
	}
	if len(r.ImageURLs) > 0 && (!config.Edit || len(r.ImageURLs) > config.MaxReferences) {
		return false
	}
	return r.MaskURL == "" || config.Mask
}

func imageModelCandidates(db *gorm.DB, group, id string) ([]*Channel, error) {
	var abilities []Ability
	query := db.Where(map[string]any{"group": group, "enabled": true})
	if id != "" {
		query = query.Where("model = ?", imageModelChannelName(id))
	} else {
		query = query.Where("model LIKE ?", imageModelChannelPrefix+"%")
	}
	if err := query.Find(&abilities).Error; err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(abilities))
	for _, ability := range abilities {
		ids = append(ids, ability.ChannelId)
	}
	var channels []*Channel
	if err := db.Where("id IN ? AND status = ?", ids, common.ChannelStatusEnabled).Find(&channels).Error; err != nil {
		return nil, err
	}
	valid := channels[:0]
	for _, channel := range channels {
		runtime, err := channel.TworkRuntime()
		if err == nil && runtime == "image_model" && channel.BaseURL != nil && *channel.BaseURL != "" {
			valid = append(valid, channel)
		}
	}
	sort.SliceStable(valid, func(i, j int) bool {
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

// imageModelChannels 不跨模型、不降档：只返回能满足本次请求的同模型渠道。
func imageModelChannels(db *gorm.DB, group string, r ImageJobRequest) ([]*Channel, error) {
	channels, err := imageModelCandidates(db, group, r.Model)
	if err != nil {
		return nil, err
	}
	valid := make([]*Channel, 0, len(channels))
	for _, channel := range channels {
		if !stringsIn(strings.Split(channel.Models, ","), imageModelChannelName(r.Model)) {
			continue
		}
		config, err := channel.ImageModelConfig()
		if err == nil && config.supports(r) {
			valid = append(valid, channel)
		}
	}
	return valid, nil
}

// ListImageModels 按模型去重，排序和名称取优先级最高的渠道。
func ListImageModels(ctx context.Context, tokenID int) ([]ImageModelView, error) {
	var token Token
	if err := DB.WithContext(ctx).First(&token, tokenID).Error; err != nil {
		return nil, err
	}
	var user User
	if err := DB.WithContext(ctx).First(&user, token.UserId).Error; err != nil {
		return nil, err
	}
	group := token.Group
	if group == "" {
		group = user.Group
	}
	if !imageJobGroupAllowed(user.Group, group) {
		return nil, ErrTworkRouteDenied
	}
	channels, err := imageModelCandidates(DB.WithContext(ctx), group, "")
	if err != nil {
		return nil, err
	}
	type entry struct {
		view  ImageModelView
		order int
	}
	seen := map[string]bool{}
	entries := []entry{}
	for _, channel := range channels {
		config, err := channel.ImageModelConfig()
		if err != nil {
			continue
		}
		for _, name := range strings.Split(channel.Models, ",") {
			id := strings.TrimPrefix(name, imageModelChannelPrefix)
			if id == name || seen[id] {
				continue
			}
			seen[id] = true
			resolutions := []string{}
			for _, resolution := range []string{"1k", "2k", "4k"} {
				if config.PricesCents[resolution] > 0 {
					resolutions = append(resolutions, resolution)
				}
			}
			entries = append(entries, entry{ImageModelView{ID: id, Name: config.DisplayName, PricesCents: config.PricesCents, Resolutions: resolutions, Sizes: config.Sizes, Edit: config.Edit, Mask: config.Mask, MaxReferences: config.MaxReferences}, config.SortOrder})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].order < entries[j].order })
	views := make([]ImageModelView, 0, len(entries))
	for _, item := range entries {
		views = append(views, item.view)
	}
	return views, nil
}
