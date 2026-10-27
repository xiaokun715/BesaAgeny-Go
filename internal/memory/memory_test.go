package memory_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/besa-ai/besa-agent/internal/memory"
)

func TestContentHashOnlyStableNormalization(t *testing.T) {
	a := memory.ContentHash(memory.ScopeSession, "  用户   喜欢 中文 ")
	b := memory.ContentHash(memory.ScopeSession, "用户 喜欢 中文")
	if a != b {
		t.Fatal("空白折叠后应相同")
	}
	c := memory.ContentHash(memory.ScopeUser, "用户 喜欢 中文")
	if a == c {
		t.Fatal("不同作用域必须是不同内容")
	}
}

func TestGatesRejectSecretsAndRedactPII(t *testing.T) {
	gates := memory.Gates{Policy: memory.DefaultPolicy()}
	if r := gates.Check(context.Background(), "我的密钥是 sk-abcdefgh12345678", memory.KindFact, memory.ScopeSession); r.Allowed {
		t.Fatal("密钥形态必须拒绝")
	}
	r := gates.Check(context.Background(), "用户手机号是 13800138000，偏好短回复", memory.KindPreference, memory.ScopeSession)
	if !r.Allowed {
		t.Fatalf("PII=redact 时应放行并脱敏：%+v", r)
	}
	if strings.Contains(r.Content, "13800138000") {
		t.Fatalf("PII 未被脱敏：%s", r.Content)
	}
	if r := gates.Check(context.Background(), "短", memory.KindFact, memory.ScopeSession); r.Allowed {
		t.Fatal("过短内容应拒绝")
	}
	if r := gates.Check(context.Background(), "这是一条有效的事实内容", memory.KindFact, memory.ScopeUser); r.Allowed {
		t.Fatal("未允许的作用域应拒绝")
	}
}

func TestDecayAndMaintenance(t *testing.T) {
	policy := memory.DefaultDecayPolicy()
	old := time.Now().Add(-365 * 24 * time.Hour)
	item := memory.Item{Kind: memory.KindFact, Weight: 1, UpdatedAt: old, State: memory.StateActive}
	if policy.EffectiveWeight(item, time.Now()) >= policy.TombstoneThreshold {
		t.Fatal("一年未访问的普通记忆应衰减到阈值以下")
	}
	if policy.NextState(item, time.Now()) != memory.StateTombstone {
		t.Fatal("应转为 tombstone")
	}
}

func TestRecallFusesLexicalWhenNoEmbedder(t *testing.T) {
	store := memory.NewMemoryStore()
	policy := memory.DefaultPolicy()
	policy.Enabled = true
	policy.LLMJudge = false
	now := time.Now().UTC()
	item := memory.Item{ID: "m1", TenantID: "default", Scope: memory.ScopeSession, SessionID: "s1",
		Content: "登录接口需要校验验证码", ContentHash: memory.ContentHash(memory.ScopeSession, "登录接口需要校验验证码"),
		Kind: memory.KindFact, State: memory.StateActive, Weight: 1, Version: 1,
		ValidFrom: now, CreatedAt: now, UpdatedAt: now}
	_ = store.SaveWithVector(context.Background(), &item, nil)
	manager := memory.NewManager(store, policy, nil, nil, nil, nil)
	results, err := manager.Recall(context.Background(), "登录 验证码", memory.Ctx{Scope: memory.ScopeSession, SessionID: "s1"}, nil, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("关键词检索应命中")
	}
	if results[0].Item.ID != "m1" {
		t.Fatalf("unexpected hit: %+v", results)
	}
}
