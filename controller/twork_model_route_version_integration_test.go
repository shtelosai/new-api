package controller

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkModelRouteCompatibilityVersionAgreesAcrossSelectionAndRetry(t *testing.T) {
	for _, tc := range []struct {
		name, effort string
		versions     []string
		metaAllowed  bool
	}{
		{"缺省", "max", nil, false},
		{"旧版", "max", []string{"0.85.1"}, false},
		{"新版", "xhigh", []string{"1.0.0"}, true},
		{"未知版本", "max", []string{"1.0.1"}, false},
		{"重复", "max", []string{"1.0.0", "1.0.0"}, false},
		{"逗号合并", "max", []string{"1.0.0, 1.0.0"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var hosts []string
			var requests []map[string]any
			router, name, channels := setupResponsesFailoverTest(t, relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				require.Equal(t, "/v1/responses", req.URL.Path)
				data, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				var body map[string]any
				require.NoError(t, common.Unmarshal(data, &body))
				hosts = append(hosts, req.URL.Host)
				requests = append(requests, body)
				status, output := http.StatusOK, `{"id":"versioned-response","object":"response","status":"completed","model":"muse-spark-1.3","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"版本验收完成"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`
				if req.URL.Host == "channel-9700.test" {
					status, output = http.StatusServiceUnavailable, `{"error":{"message":"retry","type":"server_error"}}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(output)), Request: req}, nil
			}))
			mapping, err := common.Marshal(map[string]string{name: "muse-spark-1.3"})
			require.NoError(t, err)
			for i, profile := range []string{"meta", "opencode"} {
				require.NoError(t, model.DB.Model(channels[i]).Updates(map[string]any{
					"setting":       `{"twork_runtime":"pi","twork_wire_api":"responses","pass_through_body_enabled":true,"twork_pi_compatibility":"` + profile + `"}`,
					"model_mapping": string(mapping),
				}).Error)
			}
			model.InitChannelCache()
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+name+`","input":"检查版本","reasoning":{"effort":"max"},"max_output_tokens":16}`))
			for key, value := range map[string]string{
				"Content-Type": "application/json", "X-Twork-Client-Version": "4.0.0", "X-Twork-Client-Capabilities": "model-routes-v3",
				"X-Twork-Route-Mode": "model", "X-Twork-Agent-Runtime": "pi", "X-Twork-Wire-Api": "responses",
			} {
				req.Header.Set(key, value)
			}
			for _, version := range tc.versions {
				req.Header.Add("X-Twork-Pi-Compatibility-Version", version)
			}
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code, w.Body.String())
			if tc.metaAllowed {
				assert.Equal(t, []string{"channel-9700.test", "channel-9701.test"}, hosts)
				require.Len(t, requests, 2)
				assert.Equal(t, "max", requests[0]["reasoning"].(map[string]any)["effort"])
			} else {
				assert.Equal(t, []string{"channel-9701.test"}, hosts)
				require.Len(t, requests, 1)
			}
			assert.Equal(t, tc.effort, requests[len(requests)-1]["reasoning"].(map[string]any)["effort"])
			for _, body := range requests {
				assert.Equal(t, "muse-spark-1.3", body["model"])
				assert.Equal(t, float64(16), body["max_output_tokens"])
				assert.Equal(t, false, body["store"])
			}
			var response map[string]any
			require.NoError(t, common.Unmarshal(w.Body.Bytes(), &response))
			assert.Equal(t, map[string]any{"input_tokens": float64(7), "output_tokens": float64(3), "total_tokens": float64(10)}, response["usage"])
			assert.Contains(t, w.Body.String(), "版本验收完成")
		})
	}
}
