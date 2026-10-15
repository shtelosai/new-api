package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
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
	if displayMode == "submenu" && runtime != "pi" && runtime != "omp" {
		return "", errors.New("二级菜单仅支持 Pi 渠道")
	}
	ompProfile, ompPresent, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_omp_compatibility")
	if err != nil {
		return "", err
	}
	if runtime == "omp" {
		_, present, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_pi_compatibility")
		if err != nil || present {
			return "", errors.New("OMP 渠道不能保存 Pi 兼容标记")
		}
	}
	if ompPresent && (runtime != "omp" || !dto.SupportsTworkOMPCompatibility(ompProfile, wire)) {
		return "", errors.New("OMP 适配配置无效")
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
	case "image_async":
		if wirePresent {
			return "", errors.New("异步图片渠道不能配置聊天协议")
		}
		if _, _, err := channel.ImageJobProvider(); err != nil {
			return "", err
		}
		return "image_async", nil
	case "omp":
		if !dto.SupportsTworkOMPCompatibility(ompProfile, wire) {
			return "", errors.New("OMP 渠道协议无效")
		}
		return "omp", nil
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
	if runtime != "pi" && runtime != "omp" {
		return "", nil
	}
	wire, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_wire_api")
	return wire, err
}

// IsTworkOMPChannel 用于后台列表区分新版渠道；「旧版」即所有非 OMP 渠道（含 Pi、Codex、异步生图）。
func (channel *Channel) IsTworkOMPChannel() bool {
	runtime, err := channel.TworkRuntime()
	return err == nil && runtime == "omp"
}

// ApplyChannelTworkRuntimeFilter 按运行方式筛选后台列表；同时匹配网关写入的紧凑 JSON 与 CMS 写入的带空格 JSON。
func ApplyChannelTworkRuntimeFilter(query *gorm.DB, runtimeFilter string) *gorm.DB {
	compact, spaced := `%"twork_runtime":"omp"%`, `%"twork_runtime": "omp"%`
	switch runtimeFilter {
	case "omp":
		return query.Where("(setting LIKE ? OR setting LIKE ?)", compact, spaced)
	case "legacy":
		return query.Where("(setting IS NULL OR (setting NOT LIKE ? AND setting NOT LIKE ?))", compact, spaced)
	}
	return query
}

func (channel *Channel) AllowsLegacyRuntime() bool {
	runtime, err := channel.TworkRuntime()
	return err == nil && runtime == "legacy"
}

// TworkOMPFormWires 是渠道表单可为 OMP 选择的协议：Anthropic 固定 Messages，OpenAI 二选一，其余类型不能设为 OMP。
func TworkOMPFormWires(channelType int) []string {
	switch channelType {
	case constant.ChannelTypeAnthropic:
		return []string{"anthropic-messages"}
	case constant.ChannelTypeOpenAI:
		return []string{"responses", "chat-completions"}
	}
	return nil
}

// ValidateTworkOMPFormWire 只校验渠道表单写入的 OMP 协议；CMS 既有配置仍按 TworkRuntime 校验。
func (channel *Channel) ValidateTworkOMPFormWire() error {
	if channel.Setting == nil || *channel.Setting == "" {
		return nil
	}
	runtime, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_runtime")
	if err != nil || runtime != "omp" {
		return err
	}
	wire, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_wire_api")
	if err != nil {
		return err
	}
	if !slices.Contains(TworkOMPFormWires(channel.Type), wire) {
		return errors.New("该渠道类型不支持 OMP，或 OMP 协议与渠道类型不匹配")
	}
	return nil
}

// PreserveTworkSettings 保留 CMS 管理的路由标记，普通渠道表单不能覆盖其当前值。
// 例外：当前为旧版或 OMP 的渠道，表单可在两者之间切换并选择 OMP 协议；Pi、Codex、异步生图仍只由 CMS 管理。
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
	formRuntime, formRuntimePresent, err := common.CanonicalJSONStringField([]byte(incomingJSON), "twork_runtime")
	if err != nil {
		return err
	}
	formWire, _, err := common.CanonicalJSONStringField([]byte(incomingJSON), "twork_wire_api")
	if err != nil {
		return err
	}
	storedRuntime, err := existing.TworkRuntime()
	if err != nil {
		return err
	}
	formManaged := formRuntimePresent && (formRuntime == "legacy" || formRuntime == "omp") &&
		(storedRuntime == "legacy" || storedRuntime == "omp")
	for _, field := range []string{"twork_runtime", "twork_wire_api", "twork_pi_compatibility", "twork_omp_compatibility", "twork_display_mode", "twork_image_provider", "twork_image_family"} {
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
	if formManaged && formRuntime == "legacy" {
		// 显式保留 legacy 标记，使事务内的二次合并保持幂等；OMP 专属的协议、适配和二级菜单一并清除。
		for _, field := range []string{"twork_wire_api", "twork_omp_compatibility", "twork_display_mode"} {
			delete(incoming, field)
		}
		incoming["twork_runtime"] = json.RawMessage(`"legacy"`)
	}
	if formManaged && formRuntime == "omp" {
		channelType := channel.Type
		if channelType == 0 {
			channelType = existing.Type
		}
		if !slices.Contains(TworkOMPFormWires(channelType), formWire) {
			return errors.New("该渠道类型不支持 OMP，或 OMP 协议与渠道类型不匹配")
		}
		incoming["twork_runtime"] = json.RawMessage(`"omp"`)
		incoming["twork_wire_api"] = json.RawMessage(strconv.Quote(formWire))
		// 由旧版切入时不继承任何展示或适配配置；换协议后已不支持的适配回到默认。
		if storedRuntime != "omp" {
			delete(incoming, "twork_display_mode")
			delete(incoming, "twork_omp_compatibility")
		} else if profile := existing.TworkOMPProfile(); profile != "" && !dto.SupportsTworkOMPCompatibility(profile, formWire) {
			delete(incoming, "twork_omp_compatibility")
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
func GetAuthorizedTworkChannel(ctx context.Context, tokenID int, modelName string, channelID int, billingModelName string, omp ...bool) (*Channel, error) {
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
	// OMP 与旧客户端渠道互不复用：OMP 只进 omp 渠道，旧显式路由只进 codex/pi 渠道。
	isOMP := len(omp) > 0 && omp[0]
	runtime, err := channel.TworkRuntime()
	if err != nil || (isOMP && runtime != "omp") || (!isOMP && runtime != "codex" && runtime != "pi") || channel.Status != common.ChannelStatusEnabled {
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

// ImageJobProvider 严格解析异步图片的提供方与模型家族，拒绝大小写、重复键及未知值。
func (channel *Channel) ImageJobProvider() (string, string, error) {
	if channel.Setting == nil {
		return "", "", errors.New("异步图片设置缺失")
	}
	provider, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_image_provider")
	if err != nil {
		return "", "", err
	}
	family, _, err := common.CanonicalJSONStringField([]byte(*channel.Setting), "twork_image_family")
	if err != nil {
		return "", "", err
	}
	if (provider != "kie" && provider != "apimart") || (family != "flare" && family != "sunburst") {
		return "", "", errors.New("异步图片提供方或模型家族无效")
	}
	return provider, family, nil
}

// OMP 只使用 CMS 明确标记为 omp 的渠道；legacy、codex、pi 渠道保留给旧客户端，不参与 OMP 选路。
func (channel *Channel) TworkOMPWireAPI() (string, error) {
	runtime, err := channel.TworkRuntime()
	if err != nil || runtime != "omp" {
		return "", err
	}
	wire, err := channel.TworkWireAPI()
	if wire == "chat_completions" {
		wire = "chat-completions"
	}
	return wire, err
}

func (channel *Channel) TworkOMPProfile() string {
	return channel.GetSetting().TworkOMPCompatibility
}
