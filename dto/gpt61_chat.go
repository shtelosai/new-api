package dto

import (
	"strings"
	"time"
)

// GPT61ChatParameters 仅描述已确认的 GPT 6.1 Chat 兼容规则，不推断未来模型或供应商别名。
type GPT61ChatParameters struct {
	Model            string
	ReasoningEffort  string
	SupportsSampling bool
}

func ResolveGPT61ChatParameters(modelName, reasoningEffort string) (GPT61ChatParameters, bool) {
	base := modelName
	if !isGPT61ChatSnapshot(base) {
		for _, effort := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
			candidate, found := strings.CutSuffix(modelName, "-"+effort)
			if found && isGPT61ChatSnapshot(candidate) {
				base = candidate
				reasoningEffort = effort
				break
			}
		}
	}
	if !isGPT61ChatSnapshot(base) {
		return GPT61ChatParameters{}, false
	}
	return GPT61ChatParameters{
		Model:           base,
		ReasoningEffort: reasoningEffort,
		// Sol 总是移除采样/概率参数；Terra 默认有推理，仅显式 none 保留。
		SupportsSampling: strings.HasPrefix(base, "gpt-6.1-terra") && reasoningEffort == "none",
	}, true
}

func isGPT61ChatSnapshot(modelName string) bool {
	for _, base := range []string{"gpt-6.1-sol", "gpt-6.1-terra"} {
		if modelName == base {
			return true
		}
		if date, found := strings.CutPrefix(modelName, base+"-"); found {
			if _, err := time.Parse(time.DateOnly, date); err == nil {
				return true
			}
		}
	}
	return false
}
