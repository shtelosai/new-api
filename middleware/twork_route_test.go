package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestTworkRouteHeaderContract(t *testing.T) {
	for _, tc := range []struct {
		name, id, version, cap, path string
		omit, duplicate              bool
		valid                        bool
	}{
		{name: "ordinary request", omit: true, valid: true},
		{name: "empty id", id: "", version: "4.0.0", cap: "model-routes-v1"},
		{name: "missing version", id: "1", cap: "model-routes-v1"},
		{name: "missing capability", id: "1", version: "4.0.0"},
		{name: "overflow id", id: "99999999999999999999999999999999", version: "4.0.0", cap: "model-routes-v1"},
		{name: "duplicate id", id: "1", version: "4.0.0", cap: "model-routes-v1", duplicate: true},
		{name: "exact capabilities", id: "1", version: "4.0.0", cap: "xmodel-routes-v1"},
		{name: "valid release", id: "1", version: "4.0.0", cap: "model-routes-v1", valid: true},
		{name: "future prerelease", id: "1", version: "4.0.1-rc.1", cap: "model-routes-v1"},
		{name: "major release", id: "1", version: "10.0.0", cap: "model-routes-v1", valid: true},
		{name: "invalid prerelease", id: "1", version: "4.0.1-01", cap: "model-routes-v1"},
		{name: "version whitespace", id: "1", version: " 4.0.0", cap: "model-routes-v1"},
		{name: "invalid build", id: "1", version: "4.0.0+", cap: "model-routes-v1"},
		{name: "trailing path", id: "1", version: "4.0.0", cap: "model-routes-v1", path: "/v1/responses/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path
			if path == "" {
				path = "/v1/responses"
			}
			req := httptest.NewRequest(http.MethodPost, path, nil)
			if !tc.omit {
				req.Header.Set("X-Twork-Channel-Id", tc.id)
			}
			if tc.duplicate {
				req.Header.Add("X-Twork-Channel-Id", tc.id)
			}
			if tc.version != "" {
				req.Header.Set("X-Twork-Client-Version", tc.version)
			}
			if tc.cap != "" {
				req.Header.Set("X-Twork-Client-Capabilities", tc.cap)
			}
			id, present, err := parseTworkChannelRoute(req)
			assert.Equal(t, !tc.omit, present)
			if tc.valid {
				assert.NoError(t, err)
				if !tc.omit {
					assert.Equal(t, 1, id)
				}
			} else {
				assert.Error(t, err)
			}
		})
	}
}
