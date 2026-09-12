package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"gorm.io/gorm"
)

// TworkRuntime 严格解析隔离标记，损坏的设置不能降级成普通渠道。
func (channel *Channel) TworkRuntime() (string, error) {
	if channel.Setting == nil || *channel.Setting == "" {
		return "legacy", nil
	}
	runtime, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_runtime")
	if err != nil {
		return "", err
	}
	wire, wirePresent, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_wire_api")
	if err != nil {
		return "", err
	}
	displayMode, displayPresent, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_display_mode")
	if err != nil {
		return "", err
	}
	if displayPresent && displayMode != "default" && displayMode != "submenu" {
		return "", errors.New("不支持的 twork_display_mode")
	}
	if displayMode == "submenu" && runtime != "pi" {
		return "", errors.New("二级菜单仅支持 Pi 渠道")
	}
	var settings dto.ChannelSettings
	if err := common.UnmarshalJsonStr(*channel.Setting, &settings); err != nil {
		return "", err
	}
	switch runtime {
	case "", "legacy":
		if wirePresent {
			return "", errors.New("原有渠道不能配置 twork_wire_api")
		}
		return "legacy", nil
	case "codex":
		if wirePresent && wire != "responses" {
			return "", errors.New("旧 Codex 标记仅兼容 Responses")
		}
		return "codex", nil
	case "pi":
		if wire != "responses" && wire != "chat_completions" {
			return "", errors.New("Pi 渠道必须指定有效的 twork_wire_api")
		}
		return "pi", nil
	default:
		return "", errors.New("不支持的 twork_runtime")
	}
}

// TworkWireAPI 的返回值只能来自已经完整校验的渠道设置。
func (channel *Channel) TworkWireAPI() (string, error) {
	runtime, err := channel.TworkRuntime()
	if err != nil {
		return "", err
	}
	if runtime == "codex" {
		return "responses", nil
	}
	if runtime != "pi" {
		return "", nil
	}
	wire, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_wire_api")
	return wire, err
}

func (channel *Channel) AllowsLegacyRuntime() bool {
	runtime, err := channel.TworkRuntime()
	return err == nil && runtime == "legacy"
}

// PreserveTworkSettings 保留 CMS 管理的路由标记，普通渠道表单不能覆盖其当前值。
func (channel *Channel) PreserveTworkSettings(existing *Channel) error {
	if channel.Setting == nil {
		return nil
	}
	incomingJSON, existingJSON := "{}", "{}"
	if *channel.Setting != "" {
		incomingJSON = *channel.Setting
	}
	if existing.Setting != nil && *existing.Setting != "" {
		existingJSON = *existing.Setting
	}
	var incoming, stored map[string]json.RawMessage
	if err := common.UnmarshalJsonStr(incomingJSON, &incoming); err != nil {
		return err
	}
	if err := common.UnmarshalJsonStr(existingJSON, &stored); err != nil {
		return err
	}
	for _, field := range []string{"twork_runtime", "twork_wire_api", "twork_pi_compatibility", "twork_display_mode"} {
		if _, _, err := common.CanonicalJSONStringField([]byte(incomingJSON), field); err != nil {
			return err
		}
		if _, _, err := common.CanonicalJSONStringField([]byte(existingJSON), field); err != nil {
			return err
		}
		delete(incoming, field)
		if value, present := stored[field]; present {
			incoming[field] = value
		}
	}
	encoded, err := common.Marshal(incoming)
	if err != nil {
		return err
	}
	channel.Setting = common.GetPointer(string(encoded))
	return channel.ValidateSettings()
}

var ErrTworkRouteDenied = errors.New("没有该模型渠道的访问权限，或渠道不可用")

// GetAuthorizedTworkChannel 每次直接核验持久化授权，空缓存及管理员身份均不能放行。
func GetAuthorizedTworkChannel(ctx context.Context, tokenID int, modelName string, channelID int, billingModelName string) (*Channel, error) {
	if tokenID <= 0 || modelName == "" || channelID <= 0 {
		return nil, ErrTworkRouteDenied
	}
	if DB == nil {
		return nil, errors.New("渠道授权数据库不可用")
	}
	db := DB.WithContext(ctx)
	var channel Channel
	if err := db.First(&channel, channelID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTworkRouteDenied
		}
		return nil, err
	}
	if runtime, err := channel.TworkRuntime(); err != nil || (runtime != "codex" && runtime != "pi") || channel.Status != common.ChannelStatusEnabled {
		return nil, ErrTworkRouteDenied
	}
	declared := false
	for _, name := range strings.Split(channel.Models, ",") {
		if strings.TrimSpace(name) == modelName {
			declared = true
			break
		}
	}
	if !declared {
		return nil, ErrTworkRouteDenied
	}
	var grants int64
	if err := db.Model(&TokenModelChannel{}).Where("token_id = ? AND model_id = ? AND channel_id = ?", tokenID, modelName, channelID).Count(&grants).Error; err != nil {
		return nil, err
	}
	if grants == 0 {
		return nil, ErrTworkRouteDenied
	}
	var disabled int64
	if err := db.Model(&ChannelModelDisabled{}).Where("channel_id = ? AND model IN ?", channelID, []string{modelName, billingModelName}).Count(&disabled).Error; err != nil {
		return nil, err
	}
	if disabled > 0 {
		return nil, ErrTworkRouteDenied
	}
	return &channel, nil
}

// legacyChannelIDs 在 DB 选路的优先级计算之前过滤，避免专用渠道占据最高优先级。
func legacyChannelIDs(group, modelName string, policies ...TworkRoutePolicy) ([]int, error) {
	idsQuery := DB.Model(&Ability{}).Select("channel_id").Where(commonGroupCol+" = ? AND model = ? AND enabled = ?", group, modelName, true)
	var channels []Channel
	if err := DB.Select("id", "type", "setting").Where("id IN (?)", idsQuery).Find(&channels).Error; err != nil {
		return nil, fmt.Errorf("读取渠道运行方式失败: %w", err)
	}
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		if len(policies) > 0 && policies[0].BlocksModelChannel(&channel, modelName) {
			continue
		}
		if channel.AllowsLegacyRuntime() {
			ids = append(ids, channel.Id)
		}
	}
	return ids, nil
}
