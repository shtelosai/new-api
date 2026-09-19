package model

import (
	"context"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

// GetAuthorizedTworkImageChannel 按令牌自身权限核验图片路由，不按令牌所属管理员放行。
func GetAuthorizedTworkImageChannel(ctx context.Context, tokenID int, modelName string, channelID int, group string) (*Channel, error) {
	if DB == nil || tokenID <= 0 || channelID <= 0 || modelName == "" {
		return nil, ErrTworkRouteDenied
	}
	db := DB.WithContext(ctx)
	var token Token
	if err := db.First(&token, tokenID).Error; err != nil {
		return nil, ErrTworkRouteDenied
	}
	if token.Status != common.TokenStatusEnabled || (token.ExpiredTime != -1 && token.ExpiredTime <= time.Now().Unix()) {
		return nil, ErrTworkRouteDenied
	}
	var channel Channel
	if err := db.First(&channel, channelID).Error; err != nil {
		return nil, ErrTworkRouteDenied
	}
	if channel.Status != common.ChannelStatusEnabled || !channel.AllowsLegacyRuntime() {
		return nil, ErrTworkRouteDenied
	}
	contains := func(csv, value string) bool {
		for _, item := range strings.Split(csv, ",") {
			if strings.TrimSpace(item) == value {
				return true
			}
		}
		return false
	}
	if !contains(channel.Models, modelName) || !contains(channel.Group, group) {
		return nil, ErrTworkRouteDenied
	}
	if token.Group != "" && token.Group != group {
		return nil, ErrTworkRouteDenied
	}
	if token.ModelLimitsEnabled {
		if !contains(token.ModelLimits, modelName) {
			return nil, ErrTworkRouteDenied
		}
		var grants int64
		if err := db.Model(&TokenModelChannel{}).Where("token_id = ? AND model_id = ? AND channel_id = ?", tokenID, modelName, channelID).Count(&grants).Error; err != nil {
			return nil, err
		}
		if grants == 0 {
			return nil, ErrTworkRouteDenied
		}
	}
	var disabled int64
	if err := db.Model(&ChannelModelDisabled{}).Where("channel_id = ? AND model = ?", channelID, modelName).Count(&disabled).Error; err != nil {
		return nil, err
	}
	if disabled != 0 {
		return nil, ErrTworkRouteDenied
	}
	return &channel, nil
}
