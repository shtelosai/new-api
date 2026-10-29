package service

import (
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAffinityAuditFallbackStartsAtHighestRemainingPriority(t *testing.T) {
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	previous := common.MemoryCacheEnabled
	t.Cleanup(func() { common.MemoryCacheEnabled = previous })
	for _, cached := range []bool{false, true} {
		t.Run(common.Interface2String(cached), func(t *testing.T) {
			cleanServiceTokenRoutingTables(t)
			common.MemoryCacheEnabled = cached
			const name = "affinity-priority-audit"
			seedRetryChannel(t, 3951, name, 100)
			seedRetryChannel(t, 3952, name, 90)
			seedRetryChannel(t, 3953, name, 10)
			model.InitChannelCache()
			param := newRetrySelectionParam(name)
			// 已绑定的低优先级渠道失败，已消耗一次尝试；不能跳过最高剩余层。
			param.ExcludeChannel(3953)
			param.SetRetry(1)
			first, _, err := CacheGetRandomSatisfiedChannel(param)
			require.NoError(t, err)
			require.NotNil(t, first)
			assert.Equal(t, 3951, first.Id)
			param.ExcludeChannel(first.Id)
			param.SetRetry(2)
			second, _, err := CacheGetRandomSatisfiedChannel(param)
			require.NoError(t, err)
			require.NotNil(t, second)
			assert.Equal(t, 3952, second.Id)
			param.ExcludeChannel(second.Id)
			_, _, err = CacheGetRandomSatisfiedChannel(param)
			assert.ErrorIs(t, err, ErrNoUntriedChannel)
		})
	}
}
