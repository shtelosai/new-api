package controller

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResponsesAffinityAuditRebindsAfterLowerPriorityFails(t *testing.T) {
	var hosts []string
	broken := false
	router, name, channels := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		hosts = append(hosts, req.URL.Host)
		status, body := 200, `{"id":"ok","object":"response","status":"completed","output":[],"usage":{"input_tokens":0,"output_tokens":0}}`
		if broken && req.URL.Host == "channel-9702.test" {
			status = 503
			body = `{"error":{"type":"server_error","message":"affinity channel failed"}}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	}))
	// 先建立绑定，再模拟管理员调整优先级；健康绑定不会被自动抢回。
	for _, table := range []any{&model.Channel{}, &model.Ability{}} {
		column := "id"
		if _, ok := table.(*model.Ability); ok {
			column = "channel_id"
		}
		require.NoError(t, model.DB.Model(table).Where(column+" = ?", channels[2].Id).Update("priority", 4).Error)
	}
	model.InitChannelCache()
	first := requestResponsesFailover(router, name, "affinity-priority-audit", false)
	require.Equal(t, 200, first.Code, first.Body.String())
	assert.Equal(t, []string{"channel-9702.test"}, hosts)
	for _, table := range []any{&model.Channel{}, &model.Ability{}} {
		column := "id"
		if _, ok := table.(*model.Ability); ok {
			column = "channel_id"
		}
		require.NoError(t, model.DB.Model(table).Where(column+" = ?", channels[2].Id).Update("priority", 1).Error)
	}
	model.InitChannelCache()
	hosts = nil
	broken = true
	second := requestResponsesFailover(router, name, "affinity-priority-audit", false)
	require.Equal(t, 200, second.Code, second.Body.String())
	assert.Equal(t, []string{"channel-9702.test", "channel-9700.test"}, hosts)
	hosts = nil
	third := requestResponsesFailover(router, name, "affinity-priority-audit", false)
	require.Equal(t, 200, third.Code, third.Body.String())
	assert.Equal(t, []string{"channel-9700.test"}, hosts)
}
