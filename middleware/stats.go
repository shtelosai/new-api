package middleware

import (
	"crypto/subtle"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// HTTPStats 存储HTTP统计信息
type HTTPStats struct {
	activeConnections int64
}

var globalStats = &HTTPStats{}

var (
	httpRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "newapi_http_requests_total",
			Help: "HTTP 请求总数",
		},
		[]string{"method", "route", "status_class"},
	)
	httpDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "newapi_http_request_duration_seconds",
			Help:    "HTTP 请求耗时（秒）",
			Buckets: []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300},
		},
		[]string{"method", "route", "status_class"},
	)
	httpInFlight = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "newapi_http_requests_in_flight",
			Help: "当前正在处理的 HTTP 请求数",
		},
		func() float64 {
			return float64(atomic.LoadInt64(&globalStats.activeConnections))
		},
	)
	metricsHTTPHandler = promhttp.Handler()
)

func init() {
	prometheus.MustRegister(httpRequests, httpDuration, httpInFlight)
}

// StatsMiddleware 统计中间件
func StatsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/internal/metrics" {
			c.Next()
			return
		}

		startedAt := time.Now()
		// 增加活跃连接数
		atomic.AddInt64(&globalStats.activeConnections, 1)

		// 确保在请求结束时减少连接数
		defer func() {
			atomic.AddInt64(&globalStats.activeConnections, -1)
			route := c.FullPath()
			if route == "" {
				route = "unmatched"
			}
			statusCode := c.Writer.Status()
			statusClass := "unknown"
			if statusCode >= 100 && statusCode < 600 {
				statusClass = strconv.Itoa(statusCode/100) + "xx"
			}
			labels := []string{c.Request.Method, route, statusClass}
			httpRequests.WithLabelValues(labels...).Inc()
			httpDuration.WithLabelValues(labels...).Observe(time.Since(startedAt).Seconds())
		}()

		c.Next()
	}
}

// MetricsHandler 暴露仅供本机采集的 Prometheus 指标。
func MetricsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		token := os.Getenv("METRICS_BEARER_TOKEN")
		if token == "" {
			c.Status(http.StatusServiceUnavailable)
			return
		}
		expected := "Bearer " + token
		if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), []byte(expected)) != 1 {
			c.Header("WWW-Authenticate", "Bearer")
			c.Status(http.StatusUnauthorized)
			return
		}
		metricsHTTPHandler.ServeHTTP(c.Writer, c.Request)
	}
}

// StatsInfo 统计信息结构
type StatsInfo struct {
	ActiveConnections int64 `json:"active_connections"`
}

// GetStats 获取统计信息
func GetStats() StatsInfo {
	return StatsInfo{
		ActiveConnections: atomic.LoadInt64(&globalStats.activeConnections),
	}
}
