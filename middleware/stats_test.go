package middleware

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const metricsTestToken = "metrics-test-token"

func scrapeMetricValue(t *testing.T, router http.Handler, metricName string) float64 {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+metricsTestToken)
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusOK, recorder.Code)

	pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(metricName) + ` ([-+0-9.eE]+)$`)
	match := pattern.FindStringSubmatch(recorder.Body.String())
	require.Len(t, match, 2)
	value, err := strconv.ParseFloat(match[1], 64)
	require.NoError(t, err)
	return value
}

func TestMetricsHandlerRequiresBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("METRICS_BEARER_TOKEN", metricsTestToken)
	router := gin.New()
	router.GET("/internal/metrics", MetricsHandler())

	for _, authorization := range []string{"", "Bearer wrong-token"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/internal/metrics", nil)
		request.Header.Set("Authorization", authorization)
		router.ServeHTTP(recorder, request)

		require.Equal(t, http.StatusUnauthorized, recorder.Code)
		require.NotContains(t, recorder.Body.String(), "newapi_http_requests_in_flight")
	}
}

func TestStatsMiddlewareTracksExactInFlightRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("METRICS_BEARER_TOKEN", metricsTestToken)
	router := gin.New()
	router.Use(StatsMiddleware())
	entered := make(chan struct{})
	release := make(chan struct{})
	router.GET("/hold", func(c *gin.Context) {
		close(entered)
		<-release
		c.Status(http.StatusNoContent)
	})
	router.GET("/internal/metrics", MetricsHandler())

	before := scrapeMetricValue(t, router, "newapi_http_requests_in_flight")
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/hold", nil))
		done <- recorder
	}()
	<-entered

	during := scrapeMetricValue(t, router, "newapi_http_requests_in_flight")
	close(release)
	recorder := <-done
	after := scrapeMetricValue(t, router, "newapi_http_requests_in_flight")

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, before+1, during)
	require.Equal(t, before, after)
}

func TestStatsMiddlewareUsesRouteTemplatesAndStatusClasses(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("METRICS_BEARER_TOKEN", metricsTestToken)
	router := gin.New()
	router.Use(StatsMiddleware())
	router.GET("/items/:item_id", func(c *gin.Context) {
		c.Status(http.StatusOK)
	})
	router.GET("/internal/metrics", MetricsHandler())

	for _, path := range []string{"/items/1", "/items/2"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		require.Equal(t, http.StatusOK, recorder.Code)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/internal/metrics", nil)
	request.Header.Set("Authorization", "Bearer "+metricsTestToken)
	router.ServeHTTP(recorder, request)
	payload := recorder.Body.String()

	require.Contains(t, payload, `route="/items/:item_id"`)
	require.Contains(t, payload, `status_class="2xx"`)
	require.NotContains(t, payload, `route="/items/1"`)
	require.NotContains(t, payload, `route="/items/2"`)
	require.Contains(t, payload, "newapi_http_requests_total")
	require.Contains(t, payload, "newapi_http_request_duration_seconds_count")
}
