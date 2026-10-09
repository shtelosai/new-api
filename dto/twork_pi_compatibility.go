package dto

import (
	_ "embed"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/common"
)

//go:embed twork_pi_models.json
var tworkPiModelsJSON []byte

//go:embed twork_pi_models_1_0_0.json
var tworkPiModels100JSON []byte

//go:embed twork_pi_models_1_1_0.json
var tworkPiModels110JSON []byte

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

type tworkPiCompatibilityProfile struct {
	Defaults TworkPiChatCompatibility             `json:"defaults"`
	Wires    []string                             `json:"wires"`
	Models   map[string]TworkPiModelCompatibility `json:"models"`
}

var tworkPiModels = loadTworkPiModels(tworkPiModelsJSON, "0.85.1")
var tworkPiModels100 = loadTworkPiModels(tworkPiModels100JSON, "1.0.0")
var tworkPiModels110 = loadTworkPiModels(tworkPiModels110JSON, "1.1.0")

func loadTworkPiModels(data []byte, version string) map[string]tworkPiCompatibilityProfile {
	var catalog struct {
		PiVersion string                                 `json:"pi_version"`
		Profiles  map[string]tworkPiCompatibilityProfile `json:"profiles"`
	}
	if err := common.Unmarshal(data, &catalog); err != nil {
		panic(err)
	}
	if catalog.PiVersion != version {
		panic(fmt.Sprintf("Pi 兼容快照版本不匹配：期望 %s，实际 %s", version, catalog.PiVersion))
	}
	return catalog.Profiles
}

func tworkPiProfilesForVersion(version string) map[string]tworkPiCompatibilityProfile {
	switch version {
	case "1.1.0":
		return tworkPiModels110
	case "1.0.0":
		return tworkPiModels100
	default:
		return tworkPiModels
	}
}

func SupportsTworkPiCompatibility(profile, wire, version string) bool {
	return profile == "" || slices.Contains(tworkPiProfilesForVersion(version)[profile].Wires, wire)
}

func GetTworkPiModelCompatibility(profile, model, version string) TworkPiModelCompatibility {
	entry := tworkPiProfilesForVersion(version)[profile]
	if native, found := entry.Models[model]; found {
		return native
	}
	return TworkPiModelCompatibility{Compat: entry.Defaults}
}
