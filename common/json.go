package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/tidwall/gjson"
)

func Unmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

func UnmarshalJsonStr(data string, v any) error {
	return json.Unmarshal(StringToByteSlice(data), v)
}

func DecodeJson(reader io.Reader, v any) error {
	return json.NewDecoder(reader).Decode(v)
}

func Marshal(v any) ([]byte, error) {
	return json.Marshal(v)
}

func GetJsonType(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return "unknown"
	}
	firstChar := trimmed[0]
	switch firstChar {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// JsonRawMessageToString returns JSON strings as their decoded value and other JSON values as raw text.
func JsonRawMessageToString(data json.RawMessage) string {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if trimmed[0] != '"' {
		return string(trimmed)
	}
	var value string
	if err := Unmarshal(trimmed, &value); err != nil {
		return string(trimmed)
	}
	return value
}

// CanonicalJSONStringField 读取鉴权字段，拒绝重复、大小写或转义变体，避免不同解析器得到不同身份。
func CanonicalJSONStringField(data []byte, field string) (string, bool, error) {
	root := gjson.ParseBytes(data)
	if !gjson.ValidBytes(data) || !root.IsObject() {
		return "", false, fmt.Errorf("JSON 请求或配置必须是合法对象")
	}
	var value string
	present := false
	var fieldErr error
	root.ForEach(func(key, candidate gjson.Result) bool {
		if !strings.EqualFold(key.String(), field) {
			return true
		}
		if present || key.String() != field || key.Raw != `"`+field+`"` || candidate.Type != gjson.String {
			fieldErr = fmt.Errorf("字段 %s 必须是唯一且规范的字符串键", field)
			return false
		}
		value = candidate.String()
		present = true
		return true
	})
	return value, present, fieldErr
}
