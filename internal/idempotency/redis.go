package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Redis is the default Store: one key per record, mcpgw:idem:{tenant}:{key}, with Lua scripts
// making acquire, complete and release atomic.
type Redis struct {
	client redis.UniversalClient
}

// NewRedis wraps a go-redis client.
func NewRedis(client redis.UniversalClient) *Redis { return &Redis{client: client} }

// RedisKey is the Redis key of a record.
func RedisKey(tenant, key string) string { return "mcpgw:idem:{" + tenant + "}:" + key }

// acquireScript: SET NX the in-progress record, or return the existing one.
var acquireScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if v then return v end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[2])
return false
`)

// completeScript: replace the in-progress record owned by this args hash with the result.
var completeScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then return 0 end
local r = cjson.decode(v)
if r.state ~= 'in_progress' or r.args_sha256 ~= ARGV[2] then return 0 end
redis.call('SET', KEYS[1], ARGV[1], 'PX', ARGV[3])
return 1
`)

// releaseScript: delete only an in-progress record owned by this args hash.
var releaseScript = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then return 0 end
local r = cjson.decode(v)
if r.state == 'in_progress' and r.args_sha256 == ARGV[1] then redis.call('DEL', KEYS[1]) return 1 end
return 0
`)

// Acquire implements Store.
func (r *Redis) Acquire(ctx context.Context, tenant, key, argsHash string, lockTTL time.Duration) (Acquisition, error) {
	val, _ := json.Marshal(record{State: stateInProgress, ArgsHash: argsHash})
	res, err := acquireScript.Run(ctx, r.client, []string{RedisKey(tenant, key)}, string(val), lockTTL.Milliseconds()).Result()
	if errors.Is(err, redis.Nil) {
		return Acquisition{Outcome: Acquired}, nil
	}
	if err != nil {
		return Acquisition{}, fmt.Errorf("redis acquire: %w", err)
	}
	s, ok := res.(string)
	if !ok {
		return Acquisition{}, fmt.Errorf("redis acquire: unexpected reply %T", res)
	}
	var rec record
	if err := json.Unmarshal([]byte(s), &rec); err != nil {
		return Acquisition{}, fmt.Errorf("redis acquire: decode: %w", err)
	}
	return decide(rec, argsHash), nil
}

// Complete implements Store.
func (r *Redis) Complete(ctx context.Context, tenant, key, argsHash string, result json.RawMessage, ttl time.Duration) error {
	val, err := json.Marshal(record{State: stateCompleted, ArgsHash: argsHash, Result: result})
	if err != nil {
		return fmt.Errorf("redis complete: encode: %w", err)
	}
	n, err := completeScript.Run(ctx, r.client, []string{RedisKey(tenant, key)}, string(val), argsHash, ttl.Milliseconds()).Int()
	if err != nil {
		return fmt.Errorf("redis complete: %w", err)
	}
	if n == 0 {
		return ErrNotOwner
	}
	return nil
}

// Release implements Store.
func (r *Redis) Release(ctx context.Context, tenant, key, argsHash string) error {
	if err := releaseScript.Run(ctx, r.client, []string{RedisKey(tenant, key)}, argsHash).Err(); err != nil {
		return fmt.Errorf("redis release: %w", err)
	}
	return nil
}

// Ping implements Store.
func (r *Redis) Ping(ctx context.Context) error {
	if err := r.client.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping: %w", err)
	}
	return nil
}
