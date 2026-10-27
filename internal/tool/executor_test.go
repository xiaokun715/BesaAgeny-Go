package tool_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/besa-ai/besa-agent/internal/tool"
	"github.com/besa-ai/besa-agent/internal/tool/builtin"
)

func newTestExecutor(t *testing.T, levels []string) (*tool.Executor, *tool.Registry, string) {
	t.Helper()
	dir := t.TempDir()
	reg := tool.NewRegistry()
	if err := reg.Register(builtin.NewRead()); err != nil {
		t.Fatal(err)
	}
	if err := reg.Register(builtin.NewWrite()); err != nil {
		t.Fatal(err)
	}
	permission := tool.NewPermissionPolicy(levels, map[string][]string{
		"read":  {dir},
		"write": {dir},
	})
	repo := tool.NewMemoryExecutionRepo()
	guard := tool.NewIdempotencyGuard(repo, nil, time.Minute, time.Hour, 65536, nil)
	limits := tool.DefaultLimits()
	limits.SpillDir = filepath.Join(dir, ".tool_out")
	exec := tool.NewExecutor(reg, permission, guard, nil, limits)
	return exec, reg, dir
}

func TestWriteThenReadReusesIdempotentResult(t *testing.T) {
	exec, _, dir := newTestExecutor(t, []string{"read", "write"})
	ctx := context.Background()
	target := filepath.Join(dir, "a.txt")
	inv := tool.Invocation{ToolName: "write", Arguments: map[string]any{"path": target, "content": "hello"}, Scope: "run-1"}

	first := exec.Invoke(ctx, inv)
	if first.Outcome != tool.OutcomeExecuted {
		t.Fatalf("first write should execute, got %+v", first)
	}
	second := exec.Invoke(ctx, inv)
	if second.Outcome != tool.OutcomeReused {
		t.Fatalf("second identical write should be reused, got %+v", second)
	}
	if second.SideEffectHappened() {
		t.Fatal("reused 不应报告副作用发生")
	}
	if data, _ := os.ReadFile(target); string(data) != "hello" {
		t.Fatalf("file content = %q", string(data))
	}

	read := exec.Invoke(ctx, tool.Invocation{ToolName: "read", Arguments: map[string]any{"path": target}, Scope: "run-1"})
	if read.Outcome != tool.OutcomeExecuted || read.Output != "hello" {
		t.Fatalf("read failed: %+v", read)
	}
	if !strings.Contains(read.ToModelText(), "不可信内容") {
		t.Fatalf("model text must mark untrusted content: %s", read.ToModelText())
	}
}

func TestPermissionDeniedDoesNotExecute(t *testing.T) {
	exec, _, dir := newTestExecutor(t, []string{"read"})
	target := filepath.Join(dir, "b.txt")
	result := exec.Invoke(context.Background(), tool.Invocation{ToolName: "write", Arguments: map[string]any{"path": target, "content": "x"}, Scope: "run-2"})
	if result.Outcome != tool.OutcomeRefused {
		t.Fatalf("write without permission should be refused, got %+v", result)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("被拒绝的写操作不应产生文件")
	}
}

func TestPathTraversalRefused(t *testing.T) {
	exec, _, _ := newTestExecutor(t, []string{"read", "write"})
	result := exec.Invoke(context.Background(), tool.Invocation{
		ToolName: "read", Arguments: map[string]any{"path": "../../../etc/passwd"}, Scope: "run-3",
	})
	if result.Outcome != tool.OutcomeRefused {
		t.Fatalf("path traversal should be refused, got %+v", result)
	}
}

func TestUncertainIsComputedAtReadTime(t *testing.T) {
	now := time.Now()
	repo := tool.NewMemoryExecutionRepoAt(func() time.Time { return now })
	ok, err := repo.Register(context.Background(), tool.Registration{Scope: "run-4", IdemKey: "k", ToolName: "read", SideEffect: tool.SideEffectRead})
	if err != nil || !ok {
		t.Fatalf("register failed: %v", err)
	}
	now = now.Add(10 * time.Minute)
	view, err := repo.Find(context.Background(), "run-4", "k", time.Minute)
	if err != nil || view == nil {
		t.Fatal(err)
	}
	if !view.Uncertain() {
		t.Fatalf("超租约的 in_flight 应判为 uncertain，得到 %+v", view)
	}
}

func TestSchemaViolationRefused(t *testing.T) {
	exec, _, _ := newTestExecutor(t, []string{"read"})
	result := exec.Invoke(context.Background(), tool.Invocation{ToolName: "read", Arguments: map[string]any{}, Scope: "run-5"})
	if result.Outcome != tool.OutcomeRefused || !strings.Contains(result.Error, "schema") {
		t.Fatalf("缺必需参数应被 schema 拒绝，得到 %+v", result)
	}
}

func TestIdempotencyUnavailableRefusesWrite(t *testing.T) {
	reg := tool.NewRegistry()
	_ = reg.Register(builtin.NewWrite())
	dir := t.TempDir()
	permission := tool.NewPermissionPolicy([]string{"write"}, map[string][]string{"write": {dir}})
	guard := tool.NewIdempotencyGuard(nil, nil, time.Minute, time.Hour, 65536, nil)
	exec := tool.NewExecutor(reg, permission, guard, nil, tool.DefaultLimits())
	result := exec.Invoke(context.Background(), tool.Invocation{ToolName: "write", Arguments: map[string]any{"path": filepath.Join(dir, "c.txt"), "content": "x"}, Scope: "run-6"})
	if result.Outcome != tool.OutcomeRefused {
		t.Fatalf("没有权威记录时写操作必须拒绝，得到 %+v", result)
	}
}
