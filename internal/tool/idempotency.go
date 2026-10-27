package tool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

// 幂等不可用时的默认行为：只读放行、写入拒绝。
var DefaultOnUnavailable = map[SideEffect]string{
	SideEffectRead:        "allow_with_warning",
	SideEffectWrite:       "refuse",
	SideEffectDestructive: "refuse",
}

// ArgsDigest 只做 JSON 的确定性序列化：键排序、无多余空白。
// **不对参数值做任何语义加工** —— 误去重比去重失败危险得多。
func ArgsDigest(inv Invocation) string {
	raw, _ := json.Marshal(inv.Arguments) // encoding/json 对 map 按键排序
	sum := sha256.Sum256(append([]byte(inv.ToolName+"\x00"), raw...))
	return hex.EncodeToString(sum[:])
}

// DeriveKey = 作用域 + 参数指纹。作用域必须进键：
// 同一份参数在不同运行里是不同的调用。
func DeriveKey(inv Invocation) string {
	scope := inv.Scope
	if scope == "" {
		scope = "-"
	}
	return scope + ":" + ArgsDigest(inv)
}

type ExecutionView struct {
	Scope           string
	IdemKey         string
	ToolName        string
	State           string // in_flight | done | failed | uncertain
	Owner           string
	StartedAt       time.Time
	FinishedAt      *time.Time
	Result          string
	ResultTruncated bool
	Error           string
	OutputBytes     int
}

func (v ExecutionView) Done() bool      { return v.State == "done" }
func (v ExecutionView) Retryable() bool { return v.State == "failed" }
func (v ExecutionView) Uncertain() bool { return v.State == "uncertain" }

type Registration struct {
	Scope      string
	IdemKey    string
	ToolName   string
	SideEffect SideEffect
	ArgsDigest string
	Owner      string
	TraceID    string
	SessionID  string
	Caller     string
}

// ExecutionRepo 是权威记录（Postgres）。**Redis 只是它的缓存。**
// Find 在读取时按租约把超时的 in_flight 判成 uncertain —— 不写回。
type ExecutionRepo interface {
	Find(ctx context.Context, scope, idemKey string, lease time.Duration) (*ExecutionView, error)
	Register(ctx context.Context, reg Registration) (bool, error)
	MarkDone(ctx context.Context, scope, idemKey string, result *string, truncated bool, outputBytes int) error
	MarkFailed(ctx context.Context, scope, idemKey, errMsg string) error
	Reopen(ctx context.Context, scope, idemKey, owner string) (bool, error)
}

// FastStore 是 Redis 快路径。可用性每次都要重新看 —— 它会翻转。
type FastStore interface {
	Available() bool
	Claim(ctx context.Context, key, owner string, lease time.Duration) (FastClaim, error)
	MarkDone(ctx context.Context, key string, payload []byte, ttl time.Duration) error
	Release(ctx context.Context, key, owner string) error
}

type FastClaim struct {
	State   string // claimed | in_flight | done
	Payload []byte
}

type Claim struct {
	CanExecute      bool
	Outcome         Outcome
	IdemKey         string
	Owner           string
	ToolName        string
	SideEffect      SideEffect
	Scope           string
	ReusedOutput    string
	ReusedTruncated bool
	Reason          string
}

func (c Claim) Degraded() bool { return c.Outcome == OutcomeExecutedDegraded }

func (c Claim) AsResult() Result {
	if c.Outcome == OutcomeReused {
		return Result{Outcome: OutcomeReused, ToolName: c.ToolName, Output: c.ReusedOutput, Truncated: c.ReusedTruncated, IdemKey: c.IdemKey}
	}
	return Result{Outcome: c.Outcome, ToolName: c.ToolName, Error: c.Reason, IdemKey: c.IdemKey}
}

type IdempotencyGuard struct {
	repo          ExecutionRepo
	fast          FastStore
	lease         time.Duration
	resultTTL     time.Duration
	maxCached     int
	onUnavailable map[SideEffect]string
}

func NewIdempotencyGuard(repo ExecutionRepo, fast FastStore, lease, resultTTL time.Duration, maxCachedBytes int, onUnavailable map[SideEffect]string) *IdempotencyGuard {
	if lease <= 0 {
		lease = 120 * time.Second
	}
	if resultTTL <= 0 {
		resultTTL = time.Hour
	}
	if maxCachedBytes <= 0 {
		maxCachedBytes = 65536
	}
	policy := map[SideEffect]string{}
	for k, v := range DefaultOnUnavailable {
		policy[k] = v
	}
	for k, v := range onUnavailable {
		policy[k] = v
	}
	return &IdempotencyGuard{repo: repo, fast: fast, lease: lease, resultTTL: resultTTL, maxCached: maxCachedBytes, onUnavailable: policy}
}

func (g *IdempotencyGuard) Claim(ctx context.Context, inv Invocation, effect SideEffect) Claim {
	key := inv.IdempotencyKey
	if key == "" {
		key = DeriveKey(inv)
	}
	owner := ids.Token()
	base := Claim{IdemKey: key, Owner: owner, ToolName: inv.ToolName, SideEffect: effect, Scope: inv.Scope}

	if g.repo == nil {
		return g.unavailable(base, effect, "没有配置数据库（幂等需要权威记录）")
	}

	if g.fast != nil && g.fast.Available() {
		state, err := g.fast.Claim(ctx, key, owner, g.lease)
		if err != nil {
			slog.Warn("Redis 中途不可用，退到只用权威记录的慢路径（去重仍然生效）", "err", err)
			state = FastClaim{}
		}
		switch state.State {
		case "done":
			output, truncated := decodePayload(state.Payload)
			reason := ""
			if truncated {
				reason = "结果超过缓存上限，内容未缓存（去重仍然生效）"
			}
			base.CanExecute = false
			base.Outcome = OutcomeReused
			base.ReusedOutput = output
			base.ReusedTruncated = truncated
			base.Reason = reason
			return base
		case "in_flight":
			base.CanExecute = false
			base.Outcome = OutcomeInFlight
			base.Reason = "同一个调用正在执行中（并发重复被挡住）"
			return base
		}
	}

	return g.confirmWithAuthority(ctx, inv, base)
}

func (g *IdempotencyGuard) confirmWithAuthority(ctx context.Context, inv Invocation, base Claim) Claim {
	existing, err := g.repo.Find(ctx, inv.Scope, base.IdemKey, g.lease)
	if err != nil {
		slog.Warn("权威记录不可用", "err", err)
		return g.unavailable(base, base.SideEffect, fmt.Sprintf("数据库不可用（%T）", err))
	}
	if existing != nil && existing.Done() {
		g.backfill(ctx, *existing)
		base.CanExecute = false
		base.Outcome = OutcomeReused
		base.ReusedOutput = existing.Result
		base.ReusedTruncated = existing.ResultTruncated
		base.Reason = "权威记录显示已完成（Redis 无记录或缓存已过期）"
		return base
	}
	if existing != nil && (existing.State == "in_flight" || existing.State == "uncertain") {
		if g.fast != nil && g.fast.Available() {
			_ = g.fast.Release(ctx, base.IdemKey, base.Owner)
		}
		base.CanExecute = false
		if existing.Uncertain() {
			base.Outcome = OutcomeUncertain
			base.Reason = "权威记录显示上一次执行没有正常结束（崩溃窗口）—— 副作用到底发生没有**不知道**，重试是上层的决定"
		} else {
			base.Outcome = OutcomeInFlight
			base.Reason = "权威记录显示另一个执行者正在进行"
		}
		return base
	}
	if existing != nil && existing.Retryable() {
		ok, err := g.repo.Reopen(ctx, inv.Scope, base.IdemKey, base.Owner)
		if err == nil && ok {
			base.CanExecute = true
			base.Outcome = OutcomeExecuted
			return base
		}
	}

	ok, err := g.repo.Register(ctx, Registration{
		Scope: inv.Scope, IdemKey: base.IdemKey, ToolName: inv.ToolName, SideEffect: base.SideEffect,
		ArgsDigest: ArgsDigest(inv), Owner: base.Owner, TraceID: inv.TraceID,
		SessionID: inv.SessionID, Caller: inv.Caller,
	})
	if err != nil {
		slog.Warn("权威记录登记失败", "err", err)
		return g.unavailable(base, base.SideEffect, fmt.Sprintf("数据库不可用（%T）", err))
	}
	if !ok {
		if g.fast != nil && g.fast.Available() {
			_ = g.fast.Release(ctx, base.IdemKey, base.Owner)
		}
		base.CanExecute = false
		base.Outcome = OutcomeInFlight
		base.Reason = "并发登记冲突：另一个执行者刚刚抢先登记"
		return base
	}
	base.CanExecute = true
	base.Outcome = OutcomeExecuted
	return base
}

func (g *IdempotencyGuard) backfill(ctx context.Context, view ExecutionView) {
	if g.fast == nil || !g.fast.Available() {
		return
	}
	payload, _ := encodePayload(view.Result, view.ResultTruncated)
	if err := g.fast.MarkDone(ctx, view.IdemKey, payload, g.resultTTL); err != nil {
		slog.Debug("结果缓存回填失败", "err", err)
	}
}

func (g *IdempotencyGuard) MarkDone(ctx context.Context, claim Claim, result Result) error {
	cacheable := len([]byte(result.Output)) <= g.maxCached
	if g.repo != nil {
		var stored *string
		if cacheable {
			s := result.Output
			stored = &s
		}
		err := g.repo.MarkDone(ctx, claim.Scope, claim.IdemKey, stored, result.Truncated || !cacheable, len([]byte(result.Output)))
		if err != nil {
			return err
		}
	}
	if g.fast != nil && g.fast.Available() {
		payload, _ := encodePayload(result.Output, result.Truncated || !cacheable)
		return g.fast.MarkDone(ctx, claim.IdemKey, payload, g.resultTTL)
	}
	return nil
}

func (g *IdempotencyGuard) MarkFailed(ctx context.Context, claim Claim, errMsg string) {
	if g.repo != nil {
		if err := g.repo.MarkFailed(ctx, claim.Scope, claim.IdemKey, errMsg); err != nil {
			slog.Warn("登记失败状态时出错", "err", err)
		}
	}
	if g.fast != nil && g.fast.Available() {
		_ = g.fast.Release(ctx, claim.IdemKey, claim.Owner)
	}
}

func (g *IdempotencyGuard) unavailable(base Claim, effect SideEffect, missing string) Claim {
	action := g.onUnavailable[effect]
	if action == "allow_with_warning" && effect == SideEffectRead {
		slog.Warn("幂等不可用，但工具是只读的 —— 放行执行；**重复提交会真的执行多次**", "tool", base.ToolName, "reason", missing)
		base.CanExecute = true
		base.Outcome = OutcomeExecutedDegraded
		base.Reason = "幂等不可用（" + missing + "）"
		return base
	}
	base.CanExecute = false
	base.Outcome = OutcomeRefused
	base.Reason = fmt.Sprintf("幂等不可用（%s），工具 %s 的副作用等级是 %q —— **拒绝执行**。\n重复执行有副作用的工具可能产生事故，宁可停手。", missing, base.ToolName, effect)
	return base
}

func encodePayload(output string, truncated bool) ([]byte, error) {
	return json.Marshal(map[string]any{"o": output, "t": truncated})
}

func decodePayload(raw []byte) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return string(raw), false
	}
	out, _ := data["o"].(string)
	trunc, _ := data["t"].(bool)
	return out, trunc
}

// ------------------------------ 内存权威记录（单机 / 测试） ------------------------------

type MemoryExecutionRepo struct {
	mu   sync.Mutex
	rows map[string]*ExecutionView
	now  func() time.Time
}

func NewMemoryExecutionRepo() *MemoryExecutionRepo {
	return &MemoryExecutionRepo{rows: map[string]*ExecutionView{}, now: time.Now}
}

// NewMemoryExecutionRepoAt 允许注入时钟（测试租约判定）。
func NewMemoryExecutionRepoAt(now func() time.Time) *MemoryExecutionRepo {
	return &MemoryExecutionRepo{rows: map[string]*ExecutionView{}, now: now}
}

func memKey(scope, idemKey string) string { return scope + "\x00" + idemKey }

func (r *MemoryExecutionRepo) Find(_ context.Context, scope, idemKey string, lease time.Duration) (*ExecutionView, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[memKey(scope, idemKey)]
	if !ok {
		return nil, nil
	}
	copy := *row
	if copy.State == "in_flight" && r.now().Sub(copy.StartedAt) > lease {
		copy.State = "uncertain"
	}
	return &copy, nil
}

func (r *MemoryExecutionRepo) Register(_ context.Context, reg Registration) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := memKey(reg.Scope, reg.IdemKey)
	if _, exists := r.rows[key]; exists {
		return false, nil
	}
	r.rows[key] = &ExecutionView{
		Scope: reg.Scope, IdemKey: reg.IdemKey, ToolName: reg.ToolName,
		State: "in_flight", Owner: reg.Owner, StartedAt: r.now(),
	}
	return true, nil
}

func (r *MemoryExecutionRepo) MarkDone(_ context.Context, scope, idemKey string, result *string, truncated bool, outputBytes int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[memKey(scope, idemKey)]
	if !ok {
		return fmt.Errorf("要更新的记录不存在")
	}
	row.State = "done"
	row.ResultTruncated = truncated
	row.OutputBytes = outputBytes
	if result != nil {
		row.Result = *result
	} else {
		row.Result = ""
	}
	now := r.now()
	row.FinishedAt = &now
	return nil
}

func (r *MemoryExecutionRepo) MarkFailed(_ context.Context, scope, idemKey, errMsg string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[memKey(scope, idemKey)]
	if !ok {
		return fmt.Errorf("要更新的记录不存在")
	}
	row.State = "failed"
	row.Error = errMsg
	row.Result = ""
	now := r.now()
	row.FinishedAt = &now
	return nil
}

func (r *MemoryExecutionRepo) Reopen(_ context.Context, scope, idemKey, owner string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	row, ok := r.rows[memKey(scope, idemKey)]
	if !ok || row.State != "failed" {
		return false, nil
	}
	row.State = "in_flight"
	row.Owner = owner
	row.Result = ""
	row.Error = ""
	row.FinishedAt = nil
	row.StartedAt = r.now()
	return true, nil
}
