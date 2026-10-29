package limiter

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type limiterCommandHook struct {
	commands  []string
	evalError error
}

func (h *limiterCommandHook) BeforeProcess(ctx context.Context, cmd redis.Cmder) (context.Context, error) {
	h.commands = append(h.commands, cmd.Name())
	if cmd.Name() == "eval" && h.evalError != nil {
		return ctx, h.evalError
	}
	return ctx, nil
}
func (h *limiterCommandHook) AfterProcess(context.Context, redis.Cmder) error { return nil }
func (h *limiterCommandHook) BeforeProcessPipeline(ctx context.Context, _ []redis.Cmder) (context.Context, error) {
	return ctx, nil
}
func (h *limiterCommandHook) AfterProcessPipeline(context.Context, []redis.Cmder) error { return nil }

func TestRedisLimiterRecoversMissingScriptWithoutResettingBucket(t *testing.T) {
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx := context.Background()
	sha, err := client.ScriptLoad(ctx, rateLimitScript).Result()
	require.NoError(t, err)
	rl := &RedisLimiter{client: client, limitScriptSHA: sha}
	opts := []Option{WithCapacity(2), WithRate(0), WithRequested(1)}
	allowed, err := rl.Allow(ctx, "bucket", opts...)
	require.NoError(t, err)
	require.True(t, allowed)
	require.NoError(t, client.ScriptFlush(ctx).Err())
	hook := &limiterCommandHook{}
	client.AddHook(hook)
	allowed, err = rl.Allow(ctx, "bucket", opts...)
	require.NoError(t, err)
	assert.True(t, allowed)
	assert.Equal(t, []string{"evalsha", "eval"}, hook.commands)
	hook.commands = nil
	allowed, err = rl.Allow(ctx, "bucket", opts...)
	require.NoError(t, err)
	assert.False(t, allowed, "恢复脚本不能重置桶额度")
	assert.Equal(t, []string{"evalsha"}, hook.commands, "恢复后应继续走缓存脚本")
}

func TestRedisLimiterDoesNotRetryUnrelatedErrors(t *testing.T) {
	for _, mode := range []string{"wrong type", "cancelled context", "fallback fails"} {
		t.Run(mode, func(t *testing.T) {
			server := miniredis.RunT(t)
			client := redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
			t.Cleanup(func() { require.NoError(t, client.Close()) })
			ctx := context.Background()
			sha, err := client.ScriptLoad(ctx, rateLimitScript).Result()
			require.NoError(t, err)
			rl := &RedisLimiter{client: client, limitScriptSHA: sha}
			fallbackErr := errors.New("eval unavailable")
			hook := &limiterCommandHook{}
			switch mode {
			case "wrong type":
				require.NoError(t, client.Set(ctx, "bucket", "not a hash", 0).Err())
			case "cancelled context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "fallback fails":
				require.NoError(t, client.ScriptFlush(ctx).Err())
				hook.evalError = fallbackErr
			}
			client.AddHook(hook)
			allowed, err := rl.Allow(ctx, "bucket")
			assert.False(t, allowed)
			require.Error(t, err)
			assert.True(t, strings.HasPrefix(err.Error(), "rate limit failed:"))
			if mode == "fallback fails" {
				assert.ErrorIs(t, err, fallbackErr)
				assert.Equal(t, []string{"evalsha", "eval"}, hook.commands)
			} else {
				assert.Equal(t, []string{"evalsha"}, hook.commands)
				if mode == "cancelled context" {
					assert.ErrorIs(t, err, context.Canceled)
				} else {
					assert.Contains(t, err.Error(), "WRONGTYPE")
				}
			}
		})
	}
}
