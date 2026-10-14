package controller

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func submitChannelForm(t *testing.T, method string, body map[string]any) (bool, string) {
	t.Helper()
	encoded, err := common.Marshal(body)
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Set("role", common.RoleRootUser)
	c.Set("id", 1)
	c.Request = httptest.NewRequest(method, "/api/channel/", strings.NewReader(string(encoded)))
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		AddChannel(c)
	} else {
		UpdateChannel(c)
	}
	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	return response.Success, response.Message
}

func storedChannelSetting(t *testing.T, id int) map[string]any {
	t.Helper()
	var persisted model.Channel
	require.NoError(t, model.DB.First(&persisted, id).Error)
	setting := map[string]any{}
	if persisted.Setting != nil && *persisted.Setting != "" {
		require.NoError(t, common.UnmarshalJsonStr(*persisted.Setting, &setting))
	}
	return setting
}

func TestChannelFormSwitchesOnlyBetweenLegacyAndOMP(t *testing.T) {
	cases := []struct {
		name        string
		channelType int
		stored      string
		form        string
		success     bool
		want        map[string]any
	}{
		{
			name: "legacy OpenAI becomes OMP Responses", channelType: constant.ChannelTypeOpenAI, stored: `{"proxy":"http://old"}`,
			form: `{"twork_runtime":"omp","twork_wire_api":"responses","proxy":"http://new"}`, success: true,
			want: map[string]any{"twork_runtime": "omp", "twork_wire_api": "responses", "proxy": "http://new"},
		},
		{
			name: "Anthropic becomes OMP Messages", channelType: constant.ChannelTypeAnthropic, stored: ``,
			form: `{"twork_runtime":"omp","twork_wire_api":"anthropic-messages"}`, success: true,
			want: map[string]any{"twork_runtime": "omp", "twork_wire_api": "anthropic-messages"},
		},
		{
			name: "OMP back to legacy drops OMP-only settings", channelType: constant.ChannelTypeOpenAI,
			stored: `{"twork_runtime":"omp","twork_wire_api":"chat-completions","twork_omp_compatibility":"deepseek","twork_display_mode":"submenu"}`,
			form:   `{"twork_runtime":"legacy"}`, success: true, want: map[string]any{"twork_runtime": "legacy"},
		},
		{
			name: "protocol change keeps a still-supported profile", channelType: constant.ChannelTypeOpenAI,
			stored: `{"twork_runtime":"omp","twork_wire_api":"responses","twork_omp_compatibility":"openai","twork_display_mode":"submenu"}`,
			form:   `{"twork_runtime":"omp","twork_wire_api":"responses"}`, success: true,
			want:   map[string]any{"twork_runtime": "omp", "twork_wire_api": "responses", "twork_omp_compatibility": "openai", "twork_display_mode": "submenu"},
		},
		{
			name: "protocol change resets an unsupported profile", channelType: constant.ChannelTypeOpenAI,
			stored: `{"twork_runtime":"omp","twork_wire_api":"responses","twork_omp_compatibility":"openai"}`,
			form:   `{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`, success: true,
			want:   map[string]any{"twork_runtime": "omp", "twork_wire_api": "chat-completions"},
		},
		{
			name: "Anthropic cannot use Responses", channelType: constant.ChannelTypeAnthropic, stored: ``,
			form: `{"twork_runtime":"omp","twork_wire_api":"responses"}`, success: false, want: map[string]any{},
		},
		{
			name: "unsupported channel type cannot become OMP", channelType: constant.ChannelTypeGemini, stored: ``,
			form: `{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`, success: false, want: map[string]any{},
		},
		{
			name: "Pi stays CMS-managed when the form asks for OMP", channelType: constant.ChannelTypeOpenAI,
			stored: `{"twork_runtime":"pi","twork_wire_api":"responses"}`,
			form:   `{"twork_runtime":"omp","twork_wire_api":"responses"}`, success: true,
			want:   map[string]any{"twork_runtime": "pi", "twork_wire_api": "responses"},
		},
		{
			name: "form without runtime keeps stored OMP", channelType: constant.ChannelTypeOpenAI,
			stored: `{"twork_runtime":"omp","twork_wire_api":"responses"}`,
			form:   `{"proxy":"http://new"}`, success: true,
			want:   map[string]any{"twork_runtime": "omp", "twork_wire_api": "responses", "proxy": "http://new"},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := setupModelListControllerTestDB(t)
			require.NoError(t, db.AutoMigrate(&model.ChannelModelHealth{}, &model.Log{}))
			require.NoError(t, db.Create(&model.User{Id: 1, Username: "omp-form-admin", Role: common.RoleRootUser}).Error)
			id := 5100 + i
			stored := tc.stored
			require.NoError(t, db.Create(&model.Channel{Id: id, Type: tc.channelType, Name: "omp-form", Key: "test-key", Models: "m1", Group: "default", Setting: &stored}).Error)
			before := storedChannelSetting(t, id)

			success, message := submitChannelForm(t, http.MethodPut, map[string]any{"id": id, "type": tc.channelType, "name": "omp-form", "models": "m1", "group": "default", "setting": tc.form})

			require.Equal(t, tc.success, success, message)
			if tc.success {
				assert.Equal(t, tc.want, storedChannelSetting(t, id))
			} else {
				assert.Equal(t, before, storedChannelSetting(t, id))
			}
		})
	}
}

func TestAddChannelValidatesOMPProtocolAgainstType(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelHealth{}, &model.Log{}, &model.Ability{}))
	require.NoError(t, db.Create(&model.User{Id: 1, Username: "omp-add-admin", Role: common.RoleRootUser}).Error)
	add := func(channelType int, setting string) (bool, string) {
		return submitChannelForm(t, http.MethodPost, map[string]any{"mode": "single", "channel": map[string]any{
			"type": channelType, "name": "omp-new", "key": "test-key", "models": "m1", "group": "default", "status": 2, "setting": setting,
		}})
	}

	success, message := add(constant.ChannelTypeOpenAI, `{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`)
	require.True(t, success, message)
	var created model.Channel
	require.NoError(t, db.Where("name = ?", "omp-new").First(&created).Error)
	runtime, err := created.TworkRuntime()
	require.NoError(t, err)
	assert.Equal(t, "omp", runtime)

	success, _ = add(constant.ChannelTypeAnthropic, `{"twork_runtime":"omp","twork_wire_api":"responses"}`)
	assert.False(t, success)
	success, _ = add(constant.ChannelTypeGemini, `{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`)
	assert.False(t, success)
}

func TestChannelTestUsesOMPProtocolByDefault(t *testing.T) {
	setting := func(value string) *string { return &value }
	cases := []struct {
		channel  *model.Channel
		explicit string
		want     constant.EndpointType
	}{
		{&model.Channel{Type: constant.ChannelTypeOpenAI, Setting: setting(`{"twork_runtime":"omp","twork_wire_api":"responses"}`)}, "", constant.EndpointTypeOpenAIResponse},
		{&model.Channel{Type: constant.ChannelTypeOpenAI, Setting: setting(`{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`)}, "", constant.EndpointTypeOpenAI},
		{&model.Channel{Type: constant.ChannelTypeAnthropic, Setting: setting(`{"twork_runtime":"omp","twork_wire_api":"anthropic-messages"}`)}, "", constant.EndpointTypeAnthropic},
		// 「检测」按钮显式指定协议时以显式值为准。
		{&model.Channel{Type: constant.ChannelTypeOpenAI, Setting: setting(`{"twork_runtime":"omp","twork_wire_api":"chat-completions"}`)}, string(constant.EndpointTypeOpenAIResponse), constant.EndpointTypeOpenAIResponse},
		// 旧版渠道保持原有自动判断。
		{&model.Channel{Type: constant.ChannelTypeOpenAI}, "", ""},
	}
	for _, tc := range cases {
		assert.Equal(t, string(tc.want), normalizeChannelTestEndpoint(tc.channel, "m1", tc.explicit))
	}
}

func TestChannelTestSendsOMPResponsesChannelToResponsesEndpoint(t *testing.T) {
	const name = "omp-test-responses"
	channels := setupRelaySoftCooldownChannels(t, name, 1, 0, 1)
	require.NoError(t, model.DB.AutoMigrate(&model.User{}, &model.Log{}, &model.ChannelModelHealth{}))
	require.NoError(t, model.DB.Create(&model.User{Id: 1, Username: "omp-test-admin", Role: common.RoleRootUser, Group: "default"}).Error)
	require.NoError(t, model.DB.Model(channels[0]).Updates(map[string]any{"type": constant.ChannelTypeOpenAI, "setting": `{"twork_runtime":"omp","twork_wire_api":"responses"}`}).Error)
	var channel model.Channel
	require.NoError(t, model.DB.First(&channel, channels[0].Id).Error)
	if service.GetHttpClient() == nil {
		service.InitHttpClient()
	}
	client := service.GetHttpClient()
	previous := client.Transport
	t.Cleanup(func() { client.Transport = previous })
	var paths []string
	client.Transport = relayRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
		paths = append(paths, req.URL.Path)
		body := `{"id":"resp_1","object":"response","status":"completed","model":"` + name + `","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
	})

	result := testChannel(context.Background(), &channel, 1, name, "", false)

	require.NoError(t, result.localErr)
	require.Nil(t, result.newAPIError)
	assert.Equal(t, []string{"/v1/responses"}, paths)
}

func TestChannelListFiltersByTworkRuntime(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.ChannelModelHealth{}, &model.ChannelModelDisabled{}))
	settings := map[int]string{
		6101: `{"twork_runtime":"omp","twork_wire_api":"responses"}`,
		// CMS 用 Python json.dumps 写入，带空格。
		6102: `{"twork_runtime": "omp", "twork_wire_api": "chat-completions"}`,
		6103: `{"twork_runtime":"pi","twork_wire_api":"responses"}`,
		6104: `{"proxy":""}`,
		6105: ``,
	}
	tag := "qwen"
	for id, setting := range settings {
		value := setting
		require.NoError(t, db.Create(&model.Channel{Id: id, Type: constant.ChannelTypeOpenAI, Name: "list", Key: "k", Models: "m1", Group: "default", Tag: &tag, Setting: &value}).Error)
	}
	list := func(query string) []int {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/?p=1&page_size=50&"+query, nil)
		GetAllChannels(c)
		var response struct {
			Success bool `json:"success"`
			Data    struct {
				Items []struct {
					Id int `json:"id"`
				} `json:"items"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
		require.True(t, response.Success, recorder.Body.String())
		ids := make([]int, 0, len(response.Data.Items))
		for _, item := range response.Data.Items {
			ids = append(ids, item.Id)
		}
		return ids
	}

	assert.ElementsMatch(t, []int{6101, 6102}, list("twork_runtime=omp"))
	assert.ElementsMatch(t, []int{6103, 6104, 6105}, list("twork_runtime=legacy"))
	assert.ElementsMatch(t, []int{6101, 6102, 6103, 6104, 6105}, list(""))
	assert.ElementsMatch(t, []int{6101, 6102}, list("twork_runtime=omp&tag_mode=true"))
}
