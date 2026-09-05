package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLegacyRetryExhaustsWithoutEnteringDedicatedRuntime(t *testing.T) {
	// 复用当前内存数据库，初始化 DB 非缓存路径依赖的方言列名。
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	cleanServiceTokenRoutingTables(t)
	previous := common.MemoryCacheEnabled
	t.Cleanup(func() { common.MemoryCacheEnabled = previous })
	const name = "runtime-retry-isolation"
	seedRetryChannel(t, 3501, name, 0)
	seedRetryChannel(t, 3502, name, 100)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", 3502).Update("setting", `{"twork_runtime":"codex"}`).Error)
	for _, memory := range []bool{true, false} {
		common.MemoryCacheEnabled = memory
		model.InitChannelCache()
		param := newRetrySelectionParam(name)
		selected, _, err := CacheGetRandomSatisfiedChannel(param)
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.Equal(t, 3501, selected.Id)
		param.ExcludeChannel(3501)
		param.SetRetry(1)
		selected, _, err = CacheGetRandomSatisfiedChannel(param)
		assert.ErrorIs(t, err, ErrNoUntriedChannel)
		assert.Nil(t, selected)
	}
}
