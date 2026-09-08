package dto

import (
	_ "embed"
	"slices"

	"github.com/QuantumNous/new-api/common"
)

//go:embed twork_pi_models.json
var tworkPiModelsJSON []byte

type TworkPiChatCompatibility struct {
	ZaiToolStream                               bool           `json:"zaiToolStream"`
	SupportsStore                               bool           `json:"supportsStore"`
	SupportsDeveloperRole                       bool           `json:"supportsDeveloperRole"`
	SupportsReasoningEffort                     bool           `json:"supportsReasoningEffort"`
	SupportsUsageInStreaming                    bool           `json:"supportsUsageInStreaming"`
	SupportsStrictMode                          bool           `json:"supportsStrictMode"`
	RequiresToolResultName                      bool           `json:"requiresToolResultName"`
	RequiresAssistantAfterToolResult            bool           `json:"requiresAssistantAfterToolResult"`
	RequiresThinkingAsText                      bool           `json:"requiresThinkingAsText"`
	RequiresReasoningContentOnAssistantMessages bool           `json:"requiresReasoningContentOnAssistantMessages"`
	MaxTokensField                              string         `json:"maxTokensField"`
	ThinkingFormat                              string         `json:"thinkingFormat"`
	ChatTemplateKwargs                          map[string]any `json:"chatTemplateKwargs"`
	ChatTemplateArgs                            map[string]any `json:"chatTemplateArgs"`
}

type TworkPiModelCompatibility struct {
	Compat           TworkPiChatCompatibility `json:"compat"`
	ThinkingLevelMap map[string]any           `json:"thinkingLevelMap"`
	ThinkingLevels   map[string]string        `json:"thinkingLevels"`
}

var tworkPiModels = func() map[string]struct {
	Defaults TworkPiChatCompatibility             `json:"defaults"`
	Wires    []string                             `json:"wires"`
	Models   map[string]TworkPiModelCompatibility `json:"models"`
} {
	var catalog struct {
		Profiles map[string]struct {
			Defaults TworkPiChatCompatibility             `json:"defaults"`
			Wires    []string                             `json:"wires"`
			Models   map[string]TworkPiModelCompatibility `json:"models"`
		} `json:"profiles"`
	}
	if err := common.Unmarshal(tworkPiModelsJSON, &catalog); err != nil {
		panic(err)
	}
	return catalog.Profiles
}()

func SupportsTworkPiCompatibility(profile, wire string) bool {
	return profile == "" || slices.Contains(tworkPiModels[profile].Wires, wire)
}

func GetTworkPiModelCompatibility(profile, model string) TworkPiModelCompatibility {
	entry := tworkPiModels[profile]
	if native, found := entry.Models[model]; found {
		return native
	}
	return TworkPiModelCompatibility{Compat: entry.Defaults}
}
