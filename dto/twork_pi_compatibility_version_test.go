package dto

import (
	"crypto/sha256"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTworkPiLegacySnapshotRemainsFrozen(t *testing.T) {
	assert.Equal(t, "07ad86493e2768e239fbeefcbda00fe7a26a3364b64a1fa83ee7a300e905944f", fmt.Sprintf("%x", sha256.Sum256(tworkPiModelsJSON)))
	assert.Equal(t, "81b6fd61637639a4b4a447bf1a166b76640a83df32fd3bfba75913dd46060d91", fmt.Sprintf("%x", sha256.Sum256(tworkPiModels100JSON)))
}

func TestTworkPi110KeepsProviderAndThinkingChangesVersioned(t *testing.T) {
	assert.True(t, SupportsTworkPiCompatibility("azure", "chat_completions", "1.1.0"))
	// Azure 原生 Responses 是另一 API；现有 Twork 协议目录不能把它当通用 Responses。
	assert.False(t, SupportsTworkPiCompatibility("azure", "responses", "1.1.0"))
	for _, wire := range []string{"chat_completions", "responses"} {
		for _, version := range []string{"0.85.1", "1.0.0", "1.2.0", "1.1.0-beta.1", "1.1.0,1.1.0"} {
			assert.False(t, SupportsTworkPiCompatibility("azure", wire, version))
		}
	}
	for _, tc := range []struct {
		version, maxEffort string
		reasoningEffort    bool
	}{
		{"1.0.0", "high", false},
		{"1.1.0", "max", true},
	} {
		t.Run(tc.version, func(t *testing.T) {
			assert.Equal(t, tc.maxEffort, GetTworkPiModelCompatibility("openrouter", "~anthropic/claude-haiku-latest", tc.version).ThinkingLevels["max"])
			assert.Equal(t, tc.reasoningEffort, GetTworkPiModelCompatibility("together", "deepseek-ai/DeepSeek-V4-Pro-0813", tc.version).Compat.SupportsReasoningEffort)
		})
	}
}

func TestTworkPiSnapshotRejectsMismatchedVersion(t *testing.T) {
	for _, tc := range []struct {
		name, content, version string
	}{
		{"坏 JSON", `{`, "0.85.1"},
		{"缺失版本", `{"profiles":{}}`, "0.85.1"},
		{"旧数据不能标记新版", `{"pi_version":"0.85.1","profiles":{}}`, "1.0.0"},
		{"新数据不能标记旧版", `{"pi_version":"1.0.0","profiles":{}}`, "0.85.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Panics(t, func() { loadTworkPiModels([]byte(tc.content), tc.version) })
		})
	}
}

func TestTworkPiUnknownVersionKeepsLegacyModelAndDefaults(t *testing.T) {
	for _, version := range []string{"", "0.85.1", "1.0", "1.0.1", "1.0.0-beta.1", "1.0.0,1.0.0"} {
		t.Run(version, func(t *testing.T) {
			assert.Equal(t, GetTworkPiModelCompatibility("zai", "glm-5.3", "0.85.1"), GetTworkPiModelCompatibility("zai", "glm-5.3", version))
			unknown := GetTworkPiModelCompatibility("zai", "unverified-alias", version)
			assert.Equal(t, "max_tokens", unknown.Compat.MaxTokensField)
			assert.False(t, unknown.Compat.SupportsReasoningEffort)
			assert.Empty(t, unknown.ThinkingLevels)
			assert.False(t, SupportsTworkPiCompatibility("meta", "responses", version))
			assert.False(t, SupportsTworkPiCompatibility("unknown-profile", "chat_completions", version))
			assert.True(t, SupportsTworkPiCompatibility("", "responses", version))
		})
	}
}

func TestTworkPiNewSnapshotSelectsMetaAndProfileDefaults(t *testing.T) {
	assert.True(t, SupportsTworkPiCompatibility("meta", "responses", "1.0.0"))
	assert.False(t, SupportsTworkPiCompatibility("meta", "chat_completions", "1.0.0"))
	assert.False(t, SupportsTworkPiCompatibility("unknown-profile", "responses", "1.0.0"))
	for _, profile := range []string{"", "openai", "bailian", "zai"} {
		t.Run(profile, func(t *testing.T) {
			legacy := GetTworkPiModelCompatibility(profile, "unverified-alias", "0.85.1")
			current := GetTworkPiModelCompatibility(profile, "unverified-alias", "1.0.0")
			assert.True(t, legacy.Compat.SupportsStrictMode)
			assert.False(t, current.Compat.SupportsStrictMode)
			assert.Empty(t, current.ThinkingLevels)
		})
	}
	assert.Equal(t, "deepseek", GetTworkPiModelCompatibility("moonshotai-cn", "kimi-k2.5", "0.85.1").Compat.ThinkingFormat)
	assert.Equal(t, "openai", GetTworkPiModelCompatibility("moonshotai-cn", "kimi-k2.5", "1.0.0").Compat.ThinkingFormat)
}
