package relay

import (
	"encoding/json"
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/dto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"reflect"
	"testing"
)

func TestPiResponsesCopyPreservesRequestAndIsolation(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, {}, []byte(` { "内容": "原文\\n", "value": 9007199254740993 } `), {0xff, 0, 1}} {
		t.Run(string(raw), func(t *testing.T) {
			request := dto.OpenAIResponsesRequest{}
			require.NoError(t, common.UnmarshalJsonStr(`{"model":"qwen3.8-flash","reasoning":{"effort":"high"},"stream":false,"temperature":0,"max_output_tokens":16,"stream_options":{"include_usage":true}}`, &request))
			fields := reflect.ValueOf(&request).Elem()
			for i := 0; i < fields.NumField(); i++ {
				if fields.Field(i).Type() == reflect.TypeOf(json.RawMessage{}) {
					fields.Field(i).Set(reflect.ValueOf(raw))
				}
			}
			expected, err := common.DeepCopy(&request)
			require.NoError(t, err)
			copied, err := copyPiResponsesRequest(&request)
			require.NoError(t, err)
			assert.Equal(t, expected, copied)
			copyFields := reflect.ValueOf(copied).Elem()
			for i := 0; i < copyFields.NumField(); i++ {
				if copyFields.Field(i).Type() == reflect.TypeOf(json.RawMessage{}) && copyFields.Field(i).Len() > 0 {
					v := copyFields.Field(i).Interface().(json.RawMessage)
					original := fields.Field(i).Interface().(json.RawMessage)
					before := original[0]
					v[0] ^= 1
					assert.Equal(t, before, original[0], fields.Type().Field(i).Name)
					v[0] ^= 1
				}
			}
			require.NotNil(t, copied.Reasoning)
			copied.Reasoning.Effort = "low"
			assert.Equal(t, "high", request.Reasoning.Effort)
			require.NotNil(t, copied.Stream)
			*copied.Stream = true
			assert.False(t, *request.Stream)
			require.NotNil(t, copied.StreamOptions)
			copied.StreamOptions.IncludeUsage = false
			assert.True(t, request.StreamOptions.IncludeUsage)
		})
	}
}
func TestPiResponsesCopyRejectsMissingRequest(t *testing.T) {
	copied, err := copyPiResponsesRequest(nil)
	require.Error(t, err)
	assert.Nil(t, copied)
}
