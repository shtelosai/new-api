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
	if info.ChannelSetting.TworkWireAPI == "chat_completions" {
		adaptTworkChatRequest(body, info)
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
				native := dto.GetTworkPiModelCompatibility(info.ChannelSetting.TworkPiCompatibility, info.UpstreamModelName)
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
	return common.Marshal(body)
}

func adaptTworkChatRequest(body map[string]any, info *RelayInfo) {
	profile := info.ChannelSetting.TworkPiCompatibility
	native := dto.GetTworkPiModelCompatibility(profile, info.UpstreamModelName)
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
