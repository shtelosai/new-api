package common

import (
	"net/http"
	"regexp"

	"github.com/gin-gonic/gin"
)

const TworkUsageContextKey = "twork_usage"

// TworkUsage identifies a client reply or auxiliary operation, never its content.
type TworkUsage struct {
	Turn    string `json:"twork_turn,omitempty"`
	Session string `json:"twork_session,omitempty"`
	Aux     string `json:"twork_aux,omitempty"`
}

var tworkUsageHashPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func ParseTworkUsageHeaders(header http.Header) TworkUsage {
	var usage TworkUsage
	for name, target := range map[string]*string{"X-Twork-Turn": &usage.Turn, "X-Twork-Session": &usage.Session} {
		values := header.Values(name)
		if len(values) == 1 && tworkUsageHashPattern.MatchString(values[0]) {
			*target = values[0]
		}
	}
	if values := header.Values("X-Twork-Aux"); usage.Turn == "" && len(values) == 1 {
		switch values[0] {
		case "title", "composer_prediction", "memory", "prompt_enhance", "role_prompt", "one_shot", "kb_import", "avatar", "minutes", "group_chat", "lingshi":
			usage.Aux = values[0]
		}
	}
	return usage
}

func GetTworkUsage(c *gin.Context) TworkUsage {
	if c == nil {
		return TworkUsage{}
	}
	value, _ := c.Get(TworkUsageContextKey)
	usage, _ := value.(TworkUsage)
	return usage
}

// AddToOther preserves existing billing metadata and omits absent client tags.
func (usage TworkUsage) AddToOther(other map[string]interface{}) map[string]interface{} {
	if usage.Session == "" && usage.Aux == "" {
		return other
	}
	if other == nil {
		other = make(map[string]interface{})
	}
	if usage.Session != "" {
		other["twork_session"] = usage.Session
	}
	if usage.Aux != "" {
		other["twork_aux"] = usage.Aux
	}
	return other
}
