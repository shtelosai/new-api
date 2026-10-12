package common

import (
	"errors"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/gin-gonic/gin"
)

func IsTworkModelRoute(c *gin.Context) bool {
	return common.GetContextKeyString(c, constant.ContextKeyTworkRouteModel) != ""
}

// 每次从原始请求重建，避免上一渠道的映射、思考格式或覆盖参数污染下一次尝试。
func BuildTworkModelRequest(c *gin.Context, info *RelayInfo) ([]byte, error) {
	storage, err := common.GetBodyStorage(c)
	if err != nil {
		return nil, err
	}
	raw, err := storage.Bytes()
	if err != nil {
		return nil, err
	}
	var body map[string]any
	if err = common.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	body["model"] = info.UpstreamModelName
	version := common.GetContextKeyString(c, constant.ContextKeyTworkPiCompatibilityVersion)
	ompVersion := common.GetContextKeyString(c, constant.ContextKeyTworkOMPVersion)
	if ompVersion != "" {
		version = "omp:" + ompVersion
		if c.Request.URL.Path == "/v1/messages" {
			return portableOMPAnthropicRequest(body)
		}
	}
	if info.ChannelSetting.TworkWireAPI == "chat_completions" || info.ChannelSetting.TworkWireAPI == "chat-completions" || (ompVersion != "" && c.Request.URL.Path == "/v1/chat/completions") {
		adaptTworkChatRequest(body, info, version)
	} else {
		if id, _ := body["previous_response_id"].(string); id != "" {
			return nil, errors.New("模型级请求必须携带完整公开历史，不能使用渠道专属恢复 ID")
		}
		// 原生加密思考和响应 ID 只属于原渠道；公开消息及工具调用配对仍保留。
		if input, ok := body["input"].([]any); ok {
			filtered := make([]any, 0, len(input))
			for _, value := range input {
				if item, ok := value.(map[string]any); ok {
					if item["type"] == "item_reference" || item["type"] == "compaction" {
						return nil, errors.New("模型级请求必须携带完整公开历史，不能使用渠道专属历史引用或加密压缩")
					}
					if item["type"] == "reasoning" {
						// 新版保留可移植的明文摘要；渠道签名、密文及恢复状态仍不回放。
						if version == "1.0.0" || version == "1.1.0" || version == "omp:"+dto.TworkOMPVersion {
							summary, _ := item["summary"].([]any)
							for _, entry := range summary {
								part, ok := entry.(map[string]any)
								if !ok || part["type"] != "summary_text" {
									continue
								}
								if text, ok := part["text"].(string); ok && text != "" {
									filtered = append(filtered, map[string]any{"role": "assistant", "content": text})
								}
							}
						}
						continue
					}
					delete(item, "id")
				}
				filtered = append(filtered, value)
			}
			body["input"] = filtered
		}
		if c.Request.URL.Path == "/v1/responses/compact" {
			// 与现有 compact DTO 保持相同的受支持字段边界。
			for key := range body {
				switch key {
				case "model", "input", "instructions", "parallel_tool_calls", "service_tier", "prompt_cache_key", "prompt_cache_options", "prompt_cache_retention":
				default:
					delete(body, key)
				}
			}
		} else {
			body["store"] = false
			if reasoning, ok := body["reasoning"].(map[string]any); ok {
				native := tworkModelCompatibility(info, version)
				if effort, ok := reasoning["effort"].(string); ok {
					_, mapped, _ := resolveTworkThinking(native, effort)
					if mapped == nil {
						delete(reasoning, "effort")
						if len(reasoning) == 0 {
							delete(body, "reasoning")
						}
					} else {
						reasoning["effort"] = mapped
					}
				}
			}
		}
	}
	if ompVersion != "" {
		if err := applyOMPThinking(body, tworkModelCompatibility(info, version), c.GetHeader("X-Twork-Thinking-Level")); err != nil {
			return nil, err
		}
	}
	return common.Marshal(body)
}

func adaptTworkChatRequest(body map[string]any, info *RelayInfo, version string) {
	native := tworkModelCompatibility(info, version)
	compat := native.Compat
	if tools, ok := body["tools"].([]any); ok && len(tools) > 0 && compat.ZaiToolStream {
		body["tool_stream"] = true
	}
	if compat.SupportsStore == false {
		delete(body, "store")
	}
	if compat.SupportsUsageInStreaming == false {
		delete(body, "stream_options")
	}
	if compat.MaxTokensField == "max_tokens" {
		if value, ok := body["max_completion_tokens"]; ok {
			body["max_tokens"] = value
			delete(body, "max_completion_tokens")
		}
	}
	if messages, ok := body["messages"].([]any); ok {
		toolNames := map[string]string{}
		for _, value := range messages {
			message, ok := value.(map[string]any)
			if !ok {
				continue
			}
			// 加密推理只能由原渠道恢复，不能随着同模型负载均衡传给其他厂商。
			delete(message, "reasoning_details")
			if calls, ok := message["tool_calls"].([]any); ok {
				for _, value := range calls {
					if call, ok := value.(map[string]any); ok {
						if function, ok := call["function"].(map[string]any); ok {
							id, _ := call["id"].(string)
							toolNames[id], _ = function["name"].(string)
						}
					}
				}
			}
			if message["role"] == "tool" && compat.RequiresToolResultName == true {
				id, _ := message["tool_call_id"].(string)
				if name := toolNames[id]; name != "" {
					message["name"] = name
				}
			}
			if message["role"] == "developer" && compat.SupportsDeveloperRole == false {
				message["role"] = "system"
			}
			if message["role"] == "assistant" && compat.RequiresReasoningContentOnAssistantMessages == true {
				if _, exists := message["reasoning_content"]; !exists {
					message["reasoning_content"] = ""
				}
			}
		}
		if prompt := info.ChannelSetting.SystemPrompt; prompt != "" {
			found := false
			for _, value := range messages {
				message, ok := value.(map[string]any)
				if !ok || (message["role"] != "system" && message["role"] != "developer") {
					continue
				}
				found = true
				if info.ChannelSetting.SystemPromptOverride {
					if content, ok := message["content"].(string); ok {
						message["content"] = prompt + "\n" + content
					} else if content, ok := message["content"].([]any); ok {
						message["content"] = append([]any{map[string]any{"type": "text", "text": prompt}}, content...)
					}
				}
				break
			}
			if !found {
				body["messages"] = append([]any{map[string]any{"role": "system", "content": prompt}}, messages...)
			}
		}
	}
	if compat.SupportsStrictMode == false {
		if tools, ok := body["tools"].([]any); ok {
			for _, value := range tools {
				if tool, ok := value.(map[string]any); ok {
					if function, ok := tool["function"].(map[string]any); ok {
						delete(function, "strict")
					}
				}
			}
		}
	}
	if version == "omp:"+dto.TworkOMPVersion {
		return
	}
	effort, present := body["reasoning_effort"].(string)
	if !present {
		return
	}
	delete(body, "reasoning_effort")
	level, mapped, mappedPresent := resolveTworkThinking(native, effort)
	enabled := level != "off"
	if compat.SupportsReasoningEffort == true && mapped != nil && (enabled || compat.ThinkingFormat == "openai" || compat.ThinkingFormat == "baseten") {
		body["reasoning_effort"] = mapped
	}
	switch compat.ThinkingFormat {
	case "qwen":
		body["enable_thinking"] = enabled
	case "deepseek":
		if enabled || !mappedPresent || mapped != nil {
			body["thinking"] = map[string]any{"type": map[bool]string{true: "enabled", false: "disabled"}[enabled]}
		}
	case "zai":
		body["thinking"] = map[string]any{"type": "disabled"}
		if enabled {
			body["thinking"] = map[string]any{"type": "enabled", "clear_thinking": false}
		}
	case "together":
		body["reasoning"] = map[string]any{"enabled": enabled}
	case "openrouter":
		delete(body, "reasoning_effort")
		if mapped != nil {
			body["reasoning"] = map[string]any{"effort": mapped}
		}
	case "ant-ling":
		delete(body, "reasoning_effort")
		if enabled && mappedPresent && mapped != nil {
			body["reasoning"] = map[string]any{"effort": mapped}
		}
	case "qwen-chat-template":
		delete(body, "reasoning_effort")
		body["chat_template_kwargs"] = map[string]any{"enable_thinking": enabled, "preserve_thinking": true}
	case "string-thinking":
		delete(body, "reasoning_effort")
		if mapped != nil {
			body["thinking"] = mapped
		}
	case "baseten", "chat-template":
		template, key := compat.ChatTemplateArgs, "chat_template_args"
		if compat.ThinkingFormat == "chat-template" {
			template, key = compat.ChatTemplateKwargs, "chat_template_kwargs"
		}
		if template != nil {
			args := make(map[string]any, len(template))
			for key, value := range template {
				if variable, ok := value.(map[string]any); ok {
					if !enabled && variable["omitWhenOff"] == true {
						continue
					}
					if variable["$var"] == "thinking.enabled" {
						value = enabled
					} else {
						value = mapped
						if !enabled && !mappedPresent {
							value = nil
						}
					}
				}
				if value != nil {
					args[key] = value
				}
			}
			if len(args) > 0 {
				body[key] = args
			}
		}
	}
}

// 已知模型沿用 Pi SDK 的可用档位收敛；未知别名保留客户端的通用语义。
func resolveTworkThinking(native dto.TworkPiModelCompatibility, effort string) (string, any, bool) {
	level := effort
	if level == "none" {
		level = "off"
	}
	if clamped, found := native.ThinkingLevels[level]; found {
		level = clamped
	}
	mapped, present := native.ThinkingLevelMap[level]
	if !present {
		mapped = level
		if level == "off" {
			mapped = "none"
		}
	}
	return level, mapped, present
}

func tworkModelCompatibility(info *RelayInfo, version string) dto.TworkPiModelCompatibility {
	if version == "omp:"+dto.TworkOMPVersion {
		profile := info.ChannelSetting.TworkOMPCompatibility
		if info.ChannelSetting.TworkRuntime != "omp" {
			profile = info.ChannelSetting.TworkPiCompatibility
		}
		wire := info.ChannelSetting.TworkWireAPI
		if wire == "chat_completions" || (wire == "" && info.ChannelType == 1) {
			wire = "chat-completions"
		}
		return dto.GetTworkOMPModelCompatibility(profile, info.UpstreamModelName, wire)
	}
	return dto.GetTworkPiModelCompatibility(info.ChannelSetting.TworkPiCompatibility, info.UpstreamModelName, version)
}

// 模型级负载均衡不回放原渠道签名；工具配对和可见正文保持原序。
func portableOMPAnthropicRequest(body map[string]any) ([]byte, error) {
	messages, _ := body["messages"].([]any)
	for _, value := range messages {
		message, ok := value.(map[string]any)
		if !ok {
			continue
		}
		content, ok := message["content"].([]any)
		if !ok {
			continue
		}
		filtered := make([]any, 0, len(content))
		for _, value := range content {
			part, ok := value.(map[string]any)
			if !ok {
				filtered = append(filtered, value)
				continue
			}
			switch part["type"] {
			case "redacted_thinking":
				continue
			case "thinking":
				if text, ok := part["thinking"].(string); ok && text != "" {
					filtered = append(filtered, map[string]any{"type": "text", "text": text})
				}
			default:
				filtered = append(filtered, value)
			}
		}
		message["content"] = filtered
	}
	return common.Marshal(body)
}

// 思考参数由锁定 OMP 的原生序列化器生成，网关只应用对应档位的数据快照。
func applyOMPThinking(body map[string]any, native dto.TworkPiModelCompatibility, level string) error {
	if level == "" {
		return nil
	}
	params, known := native.ThinkingParams[level]
	if !known && level != "unsupported" {
		return errors.New("OMP 思考档位不受支持")
	}
	for _, key := range []string{"reasoning", "reasoning_effort", "thinking", "enable_thinking", "preserve_thinking"} {
		delete(body, key)
	}
	if kwargs, ok := body["chat_template_kwargs"].(map[string]any); ok {
		for _, key := range []string{"enable_thinking", "preserve_thinking", "reasoning_effort", "thinking"} {
			delete(kwargs, key)
		}
		if len(kwargs) == 0 {
			delete(body, "chat_template_kwargs")
		}
	}
	for key, value := range params {
		body[key] = value
	}
	return nil
}
