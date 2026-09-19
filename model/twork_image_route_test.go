package model

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTworkImageRouteChecksLiveTokenGrantAndDisable(t *testing.T) {
	cleanTokenChannelRoutingTables(t)
	require.NoError(t, DB.AutoMigrate(&Token{}))
	const name = "gpt-image-2"
	seedTokenFilterChannel(t, 291, name)
	token := Token{Id: 991, UserId: 1, Key: "image-route-test", Status: 1, ExpiredTime: -1, Group: "default", ModelLimitsEnabled: true, ModelLimits: name}
	require.NoError(t, DB.Create(&token).Error)
	t.Cleanup(func() { DB.Delete(&Token{}, 991) })
	ctx := context.Background()
	_, err := GetAuthorizedTworkImageChannel(ctx, 991, name, 291, "default")
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
	grant := TokenModelChannel{TokenId: 991, ModelId: name, ChannelId: 291}
	require.NoError(t, DB.Create(&grant).Error)
	ch, err := GetAuthorizedTworkImageChannel(ctx, 991, name, 291, "default")
	require.NoError(t, err)
	assert.Equal(t, 291, ch.Id)
	require.NoError(t, DB.Create(&ChannelModelDisabled{ChannelId: 291, Model: name, Source: "manual"}).Error)
	_, err = GetAuthorizedTworkImageChannel(ctx, 991, name, 291, "default")
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
	require.NoError(t, DB.Where("channel_id = ?", 291).Delete(&ChannelModelDisabled{}).Error)
	require.NoError(t, DB.Model(&Token{}).Where("id = ?", 991).Update("status", 2).Error)
	_, err = GetAuthorizedTworkImageChannel(ctx, 991, name, 291, "default")
	assert.ErrorIs(t, err, ErrTworkRouteDenied)
}
