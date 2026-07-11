package jimeng

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestDoRequestHonorsCanceledClientContext(t *testing.T) {
	service.InitHttpClient()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(requestCtx)

	adaptor := &Adaptor{}
	_, err := adaptor.DoRequest(c, &common.RelayInfo{
		ChannelMeta: &common.ChannelMeta{
			ChannelBaseUrl: server.URL,
			ApiKey:         "access-key|secret-key",
		},
	}, strings.NewReader(`{}`))

	require.Error(t, err)
}
