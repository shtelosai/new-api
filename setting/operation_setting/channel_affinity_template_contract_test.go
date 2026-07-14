package operation_setting

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChannelAffinityRetryPolicyTemplatesStayInSync(t *testing.T) {
	expectedRules := []struct {
		templateKey string
		name        string
		skipRetry   bool
	}{
		{templateKey: "claudeCli", name: "claude cli trace", skipRetry: false},
		{templateKey: "codexCli", name: "codex cli trace", skipRetry: true},
	}

	setting := GetChannelAffinitySetting()
	require.NotNil(t, setting)

	backendRules := make(map[string]bool, len(expectedRules))
	for _, rule := range setting.Rules {
		for _, expected := range expectedRules {
			if rule.Name == expected.name {
				backendRules[rule.Name] = rule.SkipRetryOnFailure
			}
		}
	}
	require.Len(t, backendRules, len(expectedRules))
	for _, expected := range expectedRules {
		actual, ok := backendRules[expected.name]
		require.True(t, ok, "backend rule %q not found", expected.name)
		assert.Equal(t, expected.skipRetry, actual, "backend rule %q", expected.name)
	}

	repositoryRoot, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	frontendTemplates := []struct {
		name string
		path string
	}{
		{
			name: "classic",
			path: filepath.Join(repositoryRoot, "web", "classic", "src", "constants", "channel-affinity-template.constants.js"),
		},
		{
			name: "default",
			path: filepath.Join(repositoryRoot, "web", "default", "src", "features", "system-settings", "general", "channel-affinity", "constants.ts"),
		},
	}

	for _, template := range frontendTemplates {
		t.Run(template.name, func(t *testing.T) {
			source, err := os.ReadFile(template.path)
			require.NoError(t, err)

			for _, expected := range expectedRules {
				pattern := regexp.MustCompile(fmt.Sprintf(
					`(?s)\b%s\s*:\s*\{\s*name\s*:\s*['"]%s['"],.*?\bskip_retry_on_failure\s*:\s*(true|false)\b`,
					regexp.QuoteMeta(expected.templateKey),
					regexp.QuoteMeta(expected.name),
				))
				matches := pattern.FindSubmatch(source)
				require.Len(t, matches, 2, "frontend rule %q not found in %s template", expected.name, template.name)
				assert.Equal(t, strconv.FormatBool(expected.skipRetry), string(matches[1]), "%s rule %q", template.name, expected.name)
			}
		})
	}
}
