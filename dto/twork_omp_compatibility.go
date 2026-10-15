package dto

import (
	_ "embed"
	"github.com/QuantumNous/new-api/common"
	"regexp"
	"slices"
	"strings"
)

//go:embed twork_omp_models.json
var tworkOMPModelsJSON []byte

// OMP 版本只作来源记录，不按版本拒绝请求：升级前由 Twork 核对并重新生成本快照。
var tworkOMPVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

func ValidTworkOMPVersion(version string) bool {
	return tworkOMPVersionPattern.MatchString(version)
}

// 参数解释器的版本键：OMP 请求统一为 "omp:<版本>"，Pi 客户端仍是其冻结版本号。
func IsTworkOMPCompatibilityVersion(version string) bool {
	return strings.HasPrefix(version, "omp:")
}

// 复用现有参数解释器的数据结构；版本和数据源完全独立，不回退到 Pi 快照。
var tworkOMPModels = func() map[string]tworkPiCompatibilityProfile {
	var catalog struct {
		Version  string                                 `json:"omp_version"`
		Profiles map[string]tworkPiCompatibilityProfile `json:"profiles"`
	}
	if err := common.Unmarshal(tworkOMPModelsJSON, &catalog); err != nil {
		panic(err)
	}
	return catalog.Profiles
}()

func SupportsTworkOMPCompatibility(profile, wire string) bool {
	return slices.Contains(tworkOMPModels[profile].Wires, wire)
}

func GetTworkOMPModelCompatibility(profile, model, wire string) TworkPiModelCompatibility {
	entry := tworkOMPModels[profile]
	if native, found := entry.Models[wire+":"+model]; found {
		return native
	}
	return TworkPiModelCompatibility{Compat: entry.Defaults, ThinkingParams: entry.DefaultThinkingParams[wire]}
}
