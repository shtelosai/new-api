package middleware

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOMPRequiresSingleVersionWireAndRoutingMode(t *testing.T) {
	for wire, path := range map[string]string{"anthropic-messages": "/v1/messages", "responses": "/v1/responses", "chat-completions": "/v1/chat/completions"} {
		t.Run(wire, func(t *testing.T) {
			r := httptest.NewRequest("POST", path, nil)
			headers := map[string]string{"X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3,model-routes-v4", "X-Twork-Omp-Version": "18.10.1", "X-Twork-Agent-Runtime": "omp", "X-Twork-Wire-Api": wire, "X-Twork-Route-Mode": "model"}
			for key, value := range headers {
				r.Header.Set(key, value)
			}
			policy, err := parseTworkModelRoute(r)
			require.NoError(t, err)
			assert.True(t, policy.ModelRoute)
			assert.False(t, policy.ExcludeAnthropic)
			assert.Equal(t, "18.10.1", policy.OMPVersion)
			for _, key := range []string{"X-Twork-Client-Version", "X-Twork-Client-Capabilities", "X-Twork-Omp-Version", "X-Twork-Agent-Runtime", "X-Twork-Wire-Api"} {
				r.Header.Set(key, "unknown")
				_, err = parseTworkModelRoute(r)
				assert.Error(t, err, key)
				r.Header.Set(key, headers[key])
				r.Header.Add(key, headers[key])
				_, err = parseTworkModelRoute(r)
				assert.Error(t, err, key)
				r.Header.Set(key, headers[key])
			}
			r.Header.Set("X-Twork-Channel-Id", "12")
			_, err = parseTworkModelRoute(r)
			assert.Error(t, err)
			r.Header.Del("X-Twork-Route-Mode")
			policy, err = parseTworkModelRoute(r)
			require.NoError(t, err)
			assert.False(t, policy.ModelRoute)
			id, present, err := parseTworkChannelRoute(r)
			require.NoError(t, err)
			assert.True(t, present)
			assert.Equal(t, 12, id)
			ch := &model.Channel{Type: 1, Setting: common.GetPointer(`{"twork_runtime":"omp","twork_wire_api":"` + wire + `"}`)}
			assert.True(t, tworkChannelSupportsProtocol(r, ch))
			r.Header.Set("X-Twork-Client-Capabilities", "model-routes-v2")
			r.Header.Set("X-Twork-Agent-Runtime", "pi")
			r.Header.Del("X-Twork-Omp-Version")
			assert.False(t, tworkChannelSupportsProtocol(r, ch))
		})
	}
}

// 版本号不再锁死：任意合法版本照常选路并原样记录，格式错误或多值仍拒绝。
func TestOMPAcceptsAnyWellFormedVersion(t *testing.T) {
	request := func(version ...string) *http.Request {
		r := httptest.NewRequest("POST", "/v1/responses", nil)
		for key, value := range map[string]string{"X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3,model-routes-v4", "X-Twork-Agent-Runtime": "omp", "X-Twork-Wire-Api": "responses", "X-Twork-Route-Mode": "model"} {
			r.Header.Set(key, value)
		}
		for _, value := range version {
			r.Header.Add("X-Twork-Omp-Version", value)
		}
		return r
	}
	for _, version := range []string{"18.10.1", "18.11.0", "19.0.0"} {
		policy, err := parseTworkModelRoute(request(version))
		require.NoError(t, err, version)
		assert.Equal(t, version, policy.OMPVersion)
	}
	for _, version := range [][]string{{}, {"18.11"}, {"v18.11.0"}, {"18.11.0-beta"}, {"18.10.1", "18.11.0"}} {
		_, err := parseTworkModelRoute(request(version...))
		assert.Error(t, err, version)
	}
}
