package middleware

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/model"
)

var tworkVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-((?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(?:\+([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
var tworkChannelIDPattern = regexp.MustCompile(`^[1-9][0-9]*$`)

// parseTworkChannelRoute 只识别选择意图；实际权限必须由持久化授权记录决定。
func parseTworkChannelRoute(request *http.Request) (int, bool, error) {
	values, present := request.Header[http.CanonicalHeaderKey("X-Twork-Channel-Id")]
	if !present {
		return 0, false, nil
	}
	invalid := errors.New("显式渠道请求需要有效渠道 ID、匹配的 model-routes 能力和至少 4.0.0 的正式客户端版本")
	if len(values) != 1 || !tworkChannelIDPattern.MatchString(values[0]) {
		return 0, true, invalid
	}
	id, err := strconv.Atoi(values[0])
	if err != nil {
		return 0, true, invalid
	}
	if request.URL.Path != "/v1/responses" && request.URL.Path != "/v1/responses/compact" && request.URL.Path != "/v1/chat/completions" {
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
	routeVersion := tworkRouteCapabilityVersion(request)
	if routeVersion == 0 || (request.URL.Path == "/v1/chat/completions" && routeVersion < 2) {
		return 0, true, invalid
	}
	return id, true, nil
}

func tworkRouteCapabilityVersion(request *http.Request) int {
	version := 0
	for _, capability := range strings.Split(strings.Join(request.Header.Values("X-Twork-Client-Capabilities"), ","), ",") {
		switch strings.TrimSpace(capability) {
		case "model-routes-v3":
			return 3
		case "model-routes-v2":
			version = 2
		case "model-routes-v1":
			if version < 1 {
				version = 1
			}
		}
	}
	return version
}

func parseTworkModelRoute(request *http.Request) (model.TworkRoutePolicy, error) {
	policy := model.TworkRoutePolicy{}
	versions := request.Header.Values("X-Twork-Client-Version")
	if len(versions) == 1 {
		version := tworkVersionPattern.FindStringSubmatch(versions[0])
		policy.ExcludeAnthropic = version != nil && version[4] == "" && (len(version[1]) > 1 || version[1] >= "4")
	}
	modes, present := request.Header[http.CanonicalHeaderKey("X-Twork-Route-Mode")]
	if !present {
		return policy, nil
	}
	invalid := errors.New("模型级请求需要 4.0.0 正式版、model-routes-v3、Pi 与匹配的协议，且不能指定渠道")
	_, pinned := request.Header[http.CanonicalHeaderKey("X-Twork-Channel-Id")]
	runtimes := request.Header.Values("X-Twork-Agent-Runtime")
	wires := request.Header.Values("X-Twork-Wire-Api")
	if len(modes) != 1 || modes[0] != "model" || pinned || !policy.ExcludeAnthropic || tworkRouteCapabilityVersion(request) < 3 ||
		len(runtimes) != 1 || runtimes[0] != "pi" || len(wires) != 1 {
		return policy, invalid
	}
	wire := wires[0]
	if !((wire == "responses" && (request.URL.Path == "/v1/responses" || request.URL.Path == "/v1/responses/compact")) ||
		(wire == "chat_completions" && request.URL.Path == "/v1/chat/completions")) {
		return policy, invalid
	}
	policy.ModelRoute, policy.WireAPI = true, wire
	return policy, nil
}

// 配置与请求协议必须一致；v1 客户端不能借旧请求格式进入 Pi 渠道。
func tworkChannelSupportsProtocol(request *http.Request, channel *model.Channel) bool {
	runtime, err := channel.TworkRuntime()
	if err != nil || (runtime == "pi" && tworkRouteCapabilityVersion(request) < 2) {
		return false
	}
	wire, err := channel.TworkWireAPI()
	if err != nil {
		return false
	}
	switch wire {
	case "responses":
		return request.URL.Path == "/v1/responses" || request.URL.Path == "/v1/responses/compact"
	case "chat_completions":
		return request.URL.Path == "/v1/chat/completions"
	default:
		return false
	}
}
