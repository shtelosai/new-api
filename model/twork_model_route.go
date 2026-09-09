package model

import (
	"context"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
)

// TworkRoutePolicy 是单次请求约束，不改变渠道配置或旧客户端的授权语义。
type TworkRoutePolicy struct {
	ModelRoute       bool
	ExcludeAnthropic bool
	WireAPI          string
}

func (p TworkRoutePolicy) BlocksModelChannel(channel *Channel, modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	name = name[strings.LastIndex(name, "/")+1:]
	// 4.0 的别名同样按公开模型 ID 判断，不豁免 Auto 或摘要。
	return p.ExcludeAnthropic && (channel.Type == constant.ChannelTypeAnthropic) != strings.HasPrefix(name, "claude-")
}

func (p TworkRoutePolicy) Allows(channel *Channel, modelName string) bool {
	if p.BlocksModelChannel(channel, modelName) {
		return false
	}
	if !p.ModelRoute {
		return channel.AllowsLegacyRuntime()
	}
	wire, err := channel.TworkWireAPI()
	if err != nil || wire != p.WireAPI {
		return false
	}
	profile, present, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_pi_compatibility")
	return err == nil && (!present || profile != "") && dto.SupportsTworkPiCompatibility(profile, wire)
}

// 模型级选路每次直查正式授权和启用状态；缓存开关不会放宽候选范围。
// 过滤先于优先级和权重，粘性命中也必须在同一候选池内。
func GetTworkRoutedChannel(ctx context.Context, group, modelName string, tokenID int, requestPath string, policy TworkRoutePolicy, excluded map[int]struct{}, preferredID int) (*Channel, error) {
	if policy.ModelRoute && (tokenID <= 0 || modelName == "") {
		return nil, nil
	}
	db := DB.WithContext(ctx)
	abilityIDs := db.Model(&Ability{}).Select("channel_id").Where(&Ability{Group: group, Model: modelName, Enabled: true})
	query := db.Where("id IN (?) AND status = ?", abilityIDs, common.ChannelStatusEnabled)
	if policy.ModelRoute {
		grants := db.Model(&TokenModelChannel{}).Select("channel_id").Where("token_id = ? AND model_id = ?", tokenID, modelName)
		query = query.Where("id IN (?)", grants)
	}
	disabledNames := []string{modelName, strings.TrimSuffix(modelName, "-compact")}
	if requestPath == "/v1/responses/compact" {
		disabledNames = append(disabledNames, modelName+"-compact")
	}
	disabled := db.Model(&ChannelModelDisabled{}).Select("channel_id").Where("model IN ?", disabledNames)
	query = query.Where("id NOT IN (?)", disabled)
	var channels []*Channel
	if err := query.Order("priority DESC, id ASC").Find(&channels).Error; err != nil {
		return nil, err
	}
	var candidates []*Channel
	for _, channel := range channels {
		if _, failed := excluded[channel.Id]; failed || !policy.Allows(channel, modelName) {
			continue
		}
		declared := false
		for _, name := range strings.Split(channel.Models, ",") {
			if strings.TrimSpace(name) == modelName {
				declared = true
				break
			}
		}
		if !declared || (!policy.ModelRoute && !IsChannelAllowedForToken(tokenID, modelName, channel.Id)) {
			continue
		}
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			config := channel.GetOtherSettings().AdvancedCustom
			if config == nil || !config.SupportsPathForModel(requestPath, modelName) {
				continue
			}
		}
		if channel.Id == preferredID && len(excluded) == 0 {
			return channel, nil
		}
		candidates = append(candidates, channel)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	highest := candidates[0].GetPriority()
	end := 0
	for end < len(candidates) && candidates[end].GetPriority() == highest {
		end++
	}
	return chooseChannelByWeight(candidates[:end])
}
