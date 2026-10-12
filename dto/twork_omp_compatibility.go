package dto

import (
	_ "embed"
	"github.com/QuantumNous/new-api/common"
	"slices"
)

const TworkOMPVersion = "18.10.1"

//go:embed twork_omp_models_18_10_1.json
var tworkOMPModelsJSON []byte

// 复用现有参数解释器的数据结构；版本和数据源完全独立，不回退到 Pi 快照。
var tworkOMPModels = func() map[string]tworkPiCompatibilityProfile {
	var catalog struct {
		Version  string                                 `json:"omp_version"`
		Profiles map[string]tworkPiCompatibilityProfile `json:"profiles"`
	}
	if err := common.Unmarshal(tworkOMPModelsJSON, &catalog); err != nil {
		panic(err)
	}
	if catalog.Version != TworkOMPVersion {
		panic("OMP 兼容快照版本不一致")
	}
	return catalog.Profiles
}()

func SupportsTworkOMPCompatibility(profile, wire, version string) bool {
	return version == TworkOMPVersion && slices.Contains(tworkOMPModels[profile].Wires, wire)
}

func GetTworkOMPModelCompatibility(profile, model, wire string) TworkPiModelCompatibility {
	entry := tworkOMPModels[profile]
	if native, found := entry.Models[wire+":"+model]; found {
		return native
	}
	return TworkPiModelCompatibility{Compat: entry.Defaults, ThinkingParams: entry.DefaultThinkingParams[wire]}
}
