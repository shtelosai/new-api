package service

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/require"
)

type claudeAgentProbeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f claudeAgentProbeRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func setClaudeAgentProbeTestTransport(t *testing.T, transport http.RoundTripper) {
	t.Helper()
	originalTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = transport
	t.Cleanup(func() {
		http.DefaultClient.Transport = originalTransport
	})
}

func TestProbeClaudeAgentCallsProbeService(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_PROBE_TOKEN", "test-token")
	t.Setenv("CLAUDE_AGENT_PROBE_URL", "http://probe.test")
	setClaudeAgentProbeTestTransport(t, claudeAgentProbeRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "/probe", r.URL.Path)
		require.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))

		var payload ClaudeAgentProbeRequest
		require.NoError(t, common.DecodeJson(r.Body, &payload))
		require.Equal(t, "https://example.com", payload.BaseURL)
		require.Equal(t, "claude-test", payload.Model)

		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"success":true,"latency_ms":12,"sdk_session_id":"s1"}`)),
		}, nil
	}))

	resp, err := ProbeClaudeAgent(t.Context(), ClaudeAgentProbeRequest{
		BaseURL:   "https://example.com",
		APIKey:    "sk-test",
		Model:     "claude-test",
		TimeoutMs: 1000,
	})

	require.NoError(t, err)
	require.True(t, resp.Success)
	require.Equal(t, 12, resp.LatencyMs)
	require.Equal(t, "s1", resp.SDKSessionID)
}

func TestProbeClaudeAgentReturnsServiceErrorBody(t *testing.T) {
	t.Setenv("CLAUDE_AGENT_PROBE_URL", "http://probe.test")
	setClaudeAgentProbeTestTransport(t, claudeAgentProbeRoundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"success":false,"latency_ms":0,"error":"unauthorized"}`)),
		}, nil
	}))

	resp, err := ProbeClaudeAgent(t.Context(), ClaudeAgentProbeRequest{
		BaseURL:   "https://example.com",
		APIKey:    "sk-test",
		Model:     "claude-test",
		TimeoutMs: 1000,
	})

	require.Error(t, err)
	require.NotNil(t, resp)
	require.False(t, resp.Success)
	require.Equal(t, "unauthorized", resp.Error)
}
