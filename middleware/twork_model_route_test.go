package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestModelRouteRequiresUnambiguousVersionedProtocol(t *testing.T) {
	for _, tc := range []struct {
		version, wire, path, caps string
		valid                     bool
	}{
		{"4.0.0", "chat_completions", "/v1/chat/completions", "model-routes-v1,model-routes-v2,model-routes-v3", true},
		{"4.1.0+build", "responses", "/v1/responses", "model-routes-v3", true},
		{"3.9.9", "responses", "/v1/responses", "model-routes-v3", false},
		{"4.0.0-beta.1", "responses", "/v1/responses", "model-routes-v3", false},
		{"4.0.0", "responses", "/v1/chat/completions", "model-routes-v3", false},
		{"4.0.0", "responses", "/v1/responses", "model-routes-v2", false},
	} {
		t.Run(tc.version+tc.path+tc.caps, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, tc.path, nil)
			r.Header.Set("X-Twork-Client-Version", tc.version)
			r.Header.Set("X-Twork-Client-Capabilities", tc.caps)
			r.Header.Set("X-Twork-Route-Mode", "model")
			r.Header.Set("X-Twork-Agent-Runtime", "pi")
			r.Header.Set("X-Twork-Wire-Api", tc.wire)
			policy, err := parseTworkModelRoute(r)
			if !tc.valid {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, policy.ModelRoute)
			assert.True(t, policy.ExcludeAnthropic)
			r.Header.Set("X-Twork-Channel-Id", "12")
			_, err = parseTworkModelRoute(r)
			assert.Error(t, err)
			r.Header.Del("X-Twork-Channel-Id")
			r.Header.Add("X-Twork-Wire-Api", tc.wire)
			_, err = parseTworkModelRoute(r)
			assert.Error(t, err)
		})
	}
}
