package model

import (
	"context"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPiChannelRequiresCanonicalProtocolPair(t *testing.T) {
	for _, tc := range []struct {
		setting string
		valid   bool
	}{
		{`{"twork_runtime":"pi","twork_wire_api":"responses"}`, true},
		{`{"twork_runtime":"pi","twork_wire_api":"chat_completions"}`, true},
		{`{"twork_runtime":"pi"}`, false},
		{`{"twork_runtime":"pi","twork_wire_api":null}`, false},
		{`{"twork_runtime":"pi","twork_wire_api":"messages"}`, false},
		{`{"twork_wire_api":"responses"}`, false},
		{`{"twork_runtime":"legacy","twork_wire_api":"responses"}`, false},
		{`{"twork_runtime":"codex","twork_wire_api":"chat_completions"}`, false},
		{`{"twork_runtime":"pi","twork_wire_api":"responses","twork_wire_api":"chat_completions"}`, false},
		{`{"twork_runtime":"pi","TWORK_WIRE_API":"responses"}`, false},
		{`{"twork_runtime":"pi","twork_wire_\u0061pi":"responses"}`, false},
	} {
		t.Run(tc.setting, func(t *testing.T) {
			channel := &Channel{Setting: common.GetPointer(tc.setting)}
			if tc.valid {
				assert.NoError(t, channel.ValidateSettings())
			} else {
				assert.Error(t, channel.ValidateSettings())
			}
			assert.False(t, channel.AllowsLegacyRuntime())
		})
	}
}

func TestPiAuthorizationRequiresCurrentFormalGrant(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	const name = "pi-route-model"
	seedTokenFilterChannel(t, 2451, name)
	require.NoError(t, DB.Model(&Channel{}).Where("id = ?", 2451).Update("setting", `{"twork_runtime":"pi","twork_wire_api":"chat_completions"}`).Error)
	grant := TokenModelChannel{TokenId: 773, ModelId: name, ChannelId: 2451}
	require.NoError(t, DB.Create(&grant).Error)
	ch, err := GetAuthorizedTworkChannel(context.Background(), 773, name, 2451, name)
	require.NoError(t, err)
	require.NotNil(t, ch)
	assert.Equal(t, 2451, ch.Id)
	refreshTokenModelChannelCache()
	require.NoError(t, DB.Delete(&grant).Error)
	_, err = GetAuthorizedTworkChannel(context.Background(), 773, name, 2451, name)
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
}
