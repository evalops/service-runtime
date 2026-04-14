package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

var redisTokenBucketScript = redis.NewScript(`
local key = KEYS[1]
local now_ms = tonumber(ARGV[1])
local rate = tonumber(ARGV[2])
local burst = tonumber(ARGV[3])
local ttl_ms = tonumber(ARGV[4])

local values = redis.call('HMGET', key, 'tokens', 'updated_at_ms')
local tokens = tonumber(values[1])
local updated_at_ms = tonumber(values[2])

if tokens == nil then
	tokens = burst
	updated_at_ms = now_ms
end

if now_ms > updated_at_ms then
	local elapsed_seconds = (now_ms - updated_at_ms) / 1000.0
	tokens = math.min(burst, tokens + (elapsed_seconds * rate))
	updated_at_ms = now_ms
end

local allowed = 0
local retry_after_ms = 0
if tokens >= 1 then
	tokens = tokens - 1
	allowed = 1
elseif rate > 0 then
	retry_after_ms = math.ceil(((1 - tokens) / rate) * 1000)
else
	retry_after_ms = ttl_ms
end

redis.call('HMSET', key, 'tokens', tokens, 'updated_at_ms', updated_at_ms)
redis.call('PEXPIRE', key, ttl_ms)

return {allowed, retry_after_ms}
`)

func (l *Limiter) allowRedis(ctx context.Context, policy Policy, key string, now time.Time) (bool, time.Duration, error) {
	result, err := redisTokenBucketScript.Run(
		ctx,
		l.cfg.RedisClient,
		[]string{redisBucketKey(l.cfg.ServiceName, policy.Scope, key)},
		now.UnixMilli(),
		policy.RequestsPerSecond,
		policy.Burst,
		l.entryTTL(policy).Milliseconds(),
	).Result()
	if err != nil {
		return false, 0, fmt.Errorf("redis_ratelimit: %w", err)
	}

	values, ok := result.([]interface{})
	if !ok || len(values) != 2 {
		return false, 0, fmt.Errorf("redis_ratelimit: unexpected_result %T", result)
	}

	allowed, ok := int64FromAny(values[0])
	if !ok {
		return false, 0, fmt.Errorf("redis_ratelimit: unexpected_allowed %T", values[0])
	}
	retryAfterMS, ok := int64FromAny(values[1])
	if !ok {
		return false, 0, fmt.Errorf("redis_ratelimit: unexpected_retry_after %T", values[1])
	}

	retryAfter := time.Duration(retryAfterMS) * time.Millisecond
	if retryAfter <= 0 {
		retryAfter = time.Millisecond
	}
	return allowed == 1, retryAfter, nil
}

func (l *Limiter) entryTTL(policy Policy) time.Duration {
	if l.cfg.MaxAge > 0 {
		return l.cfg.MaxAge
	}
	if policy.RequestsPerSecond <= 0 {
		return time.Minute
	}

	refill := time.Duration(math.Ceil((float64(policy.Burst) / policy.RequestsPerSecond) * float64(time.Second)))
	if refill < time.Minute {
		refill = time.Minute
	}
	return refill * 2
}

func redisBucketKey(serviceName, scope, key string) string {
	prefix := metricPrefix(serviceName)
	if strings.TrimSpace(serviceName) == "" {
		prefix = "shared"
	}
	sum := sha256.Sum256([]byte(scope + "\n" + key))
	return "ratelimit:" + prefix + ":" + hex.EncodeToString(sum[:])
}

func int64FromAny(value any) (int64, bool) {
	switch typed := value.(type) {
	case int64:
		return typed, true
	case float64:
		return int64(typed), true
	case string:
		parsed, err := strconv.ParseInt(typed, 10, 64)
		return parsed, err == nil
	case []byte:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
}
