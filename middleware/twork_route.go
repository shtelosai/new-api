package middleware

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var tworkVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
var tworkChannelIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseTworkChannelRoute 只识别选择意图；实际权限必须由持久化授权记录决定。
func parseTworkChannelRoute(request *http.Request) (int, bool, error) {
	values, present := request.Header[http.CanonicalHeaderKey("X-Twork-Channel-Id")]
	if !present {
		return 0, false, nil
	}
	invalid := errors.New("显式渠道请求需要有效渠道 ID、model-routes-v1 能力和至少 4.0.0 的客户端版本，且仅支持 Responses 接口")
	if len(values) != 1 || !tworkChannelIDPattern.MatchString(values[0]) {
		return 0, true, invalid
	}
	id, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, true, invalid
	}
	if request.URL.Path != "/v1/responses" && request.URL.Path != "/v1/responses/compact" {
		return 0, true, invalid
	}
	versions := request.Header.Values("X-Twork-Client-Version")
	if len(versions) != 1 {
		return 0, true, invalid
	}
	version := tworkVersionPattern.FindStringSubmatch(versions[0])
	if version == nil {
		return 0, true, invalid
	}
	if len(version[1]) == 1 && version[1] < "4" {
		return 0, true, invalid
	}
	if version[4] != "" {
		return 0, true, invalid
	}
	for _, capability := range strings.Split(strings.Join(request.Header.Values("X-Twork-Client-Capabilities"), ","), ",") {
		if strings.TrimSpace(capability) == "model-routes-v1" {
			return id, true, nil
		}
	}
	return 0, true, invalid
}
