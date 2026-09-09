package claudemessages

import (
	"testing"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaudeMessagesRequestToOpenAIChatPreservesGLMThinkingControls(t *testing.T) {
	request := dto.ClaudeRequest{
		Model:        "glm-5.3-flash",
		Thinking:     &dto.Thinking{Type: "enabled"},
		OutputConfig: []byte(`{"effort":"max"}`),
	}
	info := &relaycommon.RelayInfo{
		OriginModelName: "glm-5.3-flash",
		ChannelMeta: &relaycommon.ChannelMeta{
			ChannelType:       constant.ChannelTypeOpenAI,
			UpstreamModelName: "glm-5.3-flash",
		},
	}

	converted, err := ClaudeMessagesRequestToOpenAIChat(request, info)
	require.NoError(t, err)
	assert.Equal(t, "max", converted.ReasoningEffort)
	assert.JSONEq(t, `{"type":"enabled"}`, string(converted.THINKING))
}
