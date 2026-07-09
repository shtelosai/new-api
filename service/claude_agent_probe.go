package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
)

type ClaudeAgentProbeRequest struct {
	BaseURL       string            `json:"base_url"`
	APIKey        string            `json:"api_key,omitempty"`
	AuthToken     string            `json:"auth_token,omitempty"`
	Model         string            `json:"model"`
	TimeoutMs     int               `json:"timeout_ms"`
	CustomHeaders map[string]string `json:"custom_headers,omitempty"`
	Prompt        string            `json:"prompt,omitempty"`
}

type ClaudeAgentProbeResponse struct {
	Success      bool   `json:"success"`
	LatencyMs    int    `json:"latency_ms"`
	Error        string `json:"error,omitempty"`
	LocalError   bool   `json:"local_error,omitempty"`
	RawErrorType string `json:"raw_error_type,omitempty"`
	SDKSessionID string `json:"sdk_session_id,omitempty"`
	Output       string `json:"output,omitempty"`
}

func GetClaudeAgentProbeURL() string {
	return strings.TrimRight(strings.TrimSpace(common.GetEnvOrDefaultString("CLAUDE_AGENT_PROBE_URL", "")), "/")
}

func IsClaudeAgentProbeConfigured() bool {
	return GetClaudeAgentProbeURL() != ""
}

func GetClaudeAgentProbeTimeoutSeconds(defaultValue int) int {
	timeout := common.GetEnvOrDefault("CLAUDE_AGENT_PROBE_TIMEOUT_SECONDS", defaultValue)
	if timeout <= 0 {
		return defaultValue
	}
	return timeout
}

func ProbeClaudeAgent(ctx context.Context, payload ClaudeAgentProbeRequest) (*ClaudeAgentProbeResponse, error) {
	probeURL := GetClaudeAgentProbeURL()
	if probeURL == "" {
		return nil, errors.New("CLAUDE_AGENT_PROBE_URL is not configured")
	}
	if strings.TrimSpace(payload.BaseURL) == "" {
		return nil, errors.New("claude agent probe base_url is empty")
	}
	if strings.TrimSpace(payload.Model) == "" {
		return nil, errors.New("claude agent probe model is empty")
	}
	if strings.TrimSpace(payload.APIKey) == "" && strings.TrimSpace(payload.AuthToken) == "" {
		return nil, errors.New("claude agent probe api key is empty")
	}

	timeoutMs := payload.TimeoutMs
	if timeoutMs <= 0 {
		timeoutMs = GetClaudeAgentProbeTimeoutSeconds(20) * 1000
		payload.TimeoutMs = timeoutMs
	}

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutMs+2000)*time.Millisecond)
	defer cancel()

	body, err := common.Marshal(payload)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, probeURL+"/probe", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token := common.GetEnvOrDefaultString("CLAUDE_AGENT_PROBE_TOKEN", ""); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var probeResp ClaudeAgentProbeResponse
	if err := common.DecodeJson(resp.Body, &probeResp); err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if probeResp.Error == "" {
			probeResp.Error = fmt.Sprintf("claude agent probe service returned status %d", resp.StatusCode)
		}
		return &probeResp, errors.New(probeResp.Error)
	}

	return &probeResp, nil
}
