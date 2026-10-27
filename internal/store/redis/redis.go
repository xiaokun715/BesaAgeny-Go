// Package redis 提供幂等快路径、分布式限流与取消信号。
//
// **Redis 不是权威**：幂等的权威记录在 Postgres；清空 Redis 只会让它变慢，
// 不会让去重失效。限流则相反 —— 分布式部署必须用 Redis，否则多副本会让
// 实际配额超发 N 倍。
package redis

import (
	"context"
	"fmt"
	"strings"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/besa-ai/besa-agent/internal/gateway"
	"github.com/besa-ai/besa-agent/internal/tool"
)

type Client struct {
	R      *goredis.Client
	Prefix string
}

func Open(ctx context.Context, url, prefix string) (*Client, error) {
	opts, err := goredis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("解析 Redis URL: %w", err)
	}
	c := goredis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := c.Ping(pingCtx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("Redis 健康检查失败: %w", err)
	}
	if prefix == "" {
		prefix = "besa_agent:"
	}
	return &Client{R: c, Prefix: prefix}, nil
}

func (c *Client) Close() error { return c.R.Close() }

func (c *Client) key(parts ...string) string {
	return c.Prefix + strings.Join(parts, ":")
}

// ------------------------------ 幂等快路径 ------------------------------

var claimScript = goredis.NewScript(`
local payload = redis.call('GET', KEYS[1])
if payload then return {'done', payload} end
local ok = redis.call('SET', KEYS[2], ARGV[1], 'PX', ARGV[2], 'NX')
if ok then return {'claimed', ''} end
return {'in_flight', ''}
`)

var releaseScript = goredis.NewScript(`
local owner = redis.call('GET', KEYS[1])
if owner == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

func (c *Client) claimKeys(key string) (string, string) {
	return c.key("tool", "done", key), c.key("tool", "claim", key)
}

func (c *Client) FastAvailable() bool { return c != nil && c.R != nil }

// Available 实现 tool.FastStore。每次都要重新看 —— 它是会翻转的。
func (c *Client) Available() bool { return c != nil && c.R != nil }

func (c *Client) Claim(ctx context.Context, key, owner string, lease time.Duration) (tool.FastClaim, error) {
	doneKey, claimKey := c.claimKeys(key)
	res, err := claimScript.Run(ctx, c.R, []string{doneKey, claimKey}, owner, lease.Milliseconds()).Result()
	if err != nil {
		return tool.FastClaim{}, err
	}
	values, _ := res.([]any)
	if len(values) != 2 {
		return tool.FastClaim{}, fmt.Errorf("claim 脚本返回形状异常")
	}
	state := fmt.Sprint(values[0])
	payload := ""
	if s, ok := values[1].(string); ok {
		payload = s
	}
	return tool.FastClaim{State: state, Payload: []byte(payload)}, nil
}

func (c *Client) MarkDone(ctx context.Context, key string, payload []byte, ttl time.Duration) error {
	doneKey, claimKey := c.claimKeys(key)
	pipe := c.R.Pipeline()
	pipe.Set(ctx, doneKey, payload, ttl)
	pipe.Del(ctx, claimKey)
	_, err := pipe.Exec(ctx)
	return err
}

func (c *Client) Release(ctx context.Context, key, owner string) error {
	_, claimKey := c.claimKeys(key)
	return releaseScript.Run(ctx, c.R, []string{claimKey}, owner).Err()
}

// ------------------------------ 分布式限流 ------------------------------

type Limiter struct {
	client *Client
	policy gateway.RateLimitPolicy
}

func NewLimiter(client *Client, policy gateway.RateLimitPolicy) *Limiter {
	return &Limiter{client: client, policy: policy}
}

var limitScript = goredis.NewScript(`
local rpm = tonumber(ARGV[1])
local tpm = tonumber(ARGV[2])
local maxc = tonumber(ARGV[3])
local estimate = tonumber(ARGV[4])
local req = tonumber(redis.call('GET', KEYS[1]) or '0')
if rpm > 0 and req >= rpm then return 1 end
local tok = tonumber(redis.call('GET', KEYS[2]) or '0')
if tpm > 0 and tok + estimate > tpm then return 2 end
local conc = tonumber(redis.call('GET', KEYS[3]) or '0')
if maxc > 0 and conc >= maxc then return 3 end
if rpm > 0 then redis.call('INCR', KEYS[1]); redis.call('EXPIRE', KEYS[1], 120) end
if tpm > 0 then redis.call('INCRBY', KEYS[2], estimate); redis.call('EXPIRE', KEYS[2], 120) end
if maxc > 0 then redis.call('INCR', KEYS[3]); redis.call('EXPIRE', KEYS[3], 600) end
return 0
`)

func (l *Limiter) bucket(key string) (string, string, string) {
	minute := time.Now().Unix() / 60
	return l.client.key("rl", "req", key, fmt.Sprint(minute)),
		l.client.key("rl", "tok", key, fmt.Sprint(minute)),
		l.client.key("rl", "conc", key)
}

func (l *Limiter) Acquire(ctx context.Context, key string, estimated int, budget *gateway.CallBudget) (gateway.LimitDecision, error) {
	if l.policy.Empty() {
		return gateway.LimitDecision{Allowed: true}, nil
	}
	for {
		reqKey, tokKey, concKey := l.bucket(key)
		code, err := limitScript.Run(ctx, l.client.R, []string{reqKey, tokKey, concKey},
			l.policy.RPM, l.policy.TPM, l.policy.MaxConcurrency, estimated).Int()
		if err != nil {
			return gateway.LimitDecision{}, err
		}
		if code == 0 {
			return gateway.LimitDecision{Allowed: true}, nil
		}
		reason := map[int]string{1: "rpm", 2: "tpm", 3: "concurrency"}[code]
		if l.policy.OnExceed != "wait" {
			return gateway.LimitDecision{Allowed: false, Reason: reason}, nil
		}
		if remaining := budget.RemainingSeconds(); remaining <= 0.05 {
			return gateway.LimitDecision{Allowed: false, Reason: reason}, nil
		}
		select {
		case <-ctx.Done():
			return gateway.LimitDecision{Reason: "cancelled"}, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func (l *Limiter) Release(key string) {
	if l.policy.MaxConcurrency <= 0 {
		return
	}
	_, _, concKey := l.bucket(key)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// Lua 保证不会减到负数。
	_ = l.client.R.Eval(ctx, `local v=tonumber(redis.call('GET',KEYS[1]) or '0'); if v>0 then redis.call('DECR',KEYS[1]) end return v`, []string{concKey}).Err()
}

func (l *Limiter) Reconcile(key string, estimated int, actual *int) {
	if actual == nil || l.policy.TPM <= 0 {
		return
	}
	_, tokKey, _ := l.bucket(key)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = l.client.R.IncrBy(ctx, tokKey, int64(*actual-estimated)).Err()
}

// ------------------------------ 取消信号 / 锁 ------------------------------

func (c *Client) RequestCancel(ctx context.Context, runID string, ttl time.Duration) error {
	return c.R.Set(ctx, c.key("run", "cancel", runID), "1", ttl).Err()
}

func (c *Client) IsCancelled(ctx context.Context, runID string) (bool, error) {
	n, err := c.R.Exists(ctx, c.key("run", "cancel", runID)).Result()
	return n > 0, err
}

var unlockScript = goredis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end
return 0
`)

func (c *Client) TryLock(ctx context.Context, name, owner string, ttl time.Duration) (bool, error) {
	return c.R.SetNX(ctx, c.key("lock", name), owner, ttl).Result()
}

func (c *Client) Unlock(ctx context.Context, name, owner string) error {
	return unlockScript.Run(ctx, c.R, []string{c.key("lock", name)}, owner).Err()
}
