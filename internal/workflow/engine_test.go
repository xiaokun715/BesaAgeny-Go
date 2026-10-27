package workflow_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/besa-ai/besa-agent/internal/workflow"
)

type runnerFunc func(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error)

func (f runnerFunc) RunNode(ctx context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
	return f(ctx, req)
}

func twoNodePool(t *testing.T) *workflow.Pool {
	t.Helper()
	pool := workflow.NewPool()
	if err := pool.Register(workflow.AgentSpec{Name: "a", Capability: workflow.Capability{
		Tags:    []string{"a"},
		Inputs:  map[string]workflow.FieldSpec{"x": {Type: "string", Required: true}},
		Outputs: map[string]workflow.FieldSpec{"y": {Type: "string", Required: true}},
	}, Runner: runnerFunc(func(_ context.Context, _ workflow.NodeRequest) (workflow.NodeResult, error) {
		return workflow.NodeResult{Status: workflow.StateSucceeded, Outputs: map[string]any{"y": "Y"}}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	if err := pool.Register(workflow.AgentSpec{Name: "b", Capability: workflow.Capability{
		Tags:    []string{"b"},
		Inputs:  map[string]workflow.FieldSpec{"y": {Type: "string", Required: true}},
		Outputs: map[string]workflow.FieldSpec{"z": {Type: "string", Required: true}},
	}, Runner: runnerFunc(func(_ context.Context, _ workflow.NodeRequest) (workflow.NodeResult, error) {
		return workflow.NodeResult{Status: workflow.StateSucceeded, Outputs: map[string]any{"z": "Z"}}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestValidateClosureCycleAndIOMismatch(t *testing.T) {
	pool := twoNodePool(t)
	w := workflow.Workflow{WorkflowID: "w1", Nodes: []workflow.Node{
		{ID: "n1", Agent: "a", Inputs: map[string]string{"x": "x"}},
		{ID: "n2", Agent: "b"},
	}, Edges: []workflow.Edge{{Src: "n1", Dst: "n2", Kind: workflow.EdgeData, Field: "y"}}}
	vc := workflow.ValidationContext{Pool: pool, TaskInputs: map[string]workflow.FieldSpec{"x": {Type: "string", Required: true}}, Assets: []string{"x"}}
	if issues := workflow.Validate(w, vc); len(issues) != 0 {
		t.Fatalf("expected valid graph, got %+v", issues)
	}

	// 环
	w.Edges = append(w.Edges, workflow.Edge{Src: "n2", Dst: "n1", Kind: workflow.EdgeData, Field: "z"})
	issues := workflow.Validate(w, vc)
	if !hasIssue(issues, workflow.IssueCycle) {
		t.Fatalf("expected cycle issue, got %+v", issues)
	}
	w.Edges = w.Edges[:1]

	// IO 不匹配
	w.Edges[0].Field = "not_a_field"
	issues = workflow.Validate(w, vc)
	if !hasIssue(issues, workflow.IssueIOMismatch) {
		t.Fatalf("expected io mismatch, got %+v", issues)
	}
}

func TestEngineRunsDependencyOrder(t *testing.T) {
	pool := twoNodePool(t)
	var mu sync.Mutex
	var order []string
	if err := pool.Register(workflow.AgentSpec{Name: "rec", Capability: workflow.Capability{
		Outputs: map[string]workflow.FieldSpec{"y": {Type: "string", Required: true}},
	}, Runner: runnerFunc(func(_ context.Context, req workflow.NodeRequest) (workflow.NodeResult, error) {
		mu.Lock()
		order = append(order, req.Node.ID)
		mu.Unlock()
		return workflow.NodeResult{Status: workflow.StateSucceeded, Outputs: map[string]any{"y": "Y"}}, nil
	})}); err != nil {
		t.Fatal(err)
	}
	w := workflow.Workflow{WorkflowID: "w", Nodes: []workflow.Node{
		{ID: "n1", Agent: "rec"},
		{ID: "n2", Agent: "b"},
	}, Edges: []workflow.Edge{{Src: "n1", Dst: "n2", Kind: workflow.EdgeData, Field: "y"}}}
	engine := workflow.NewEngine(pool, nil, nil, nil, workflow.DefaultEnginePolicy(), nil)
	report, err := engine.Run(context.Background(), w, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != workflow.StateSucceeded {
		t.Fatalf("expected succeeded, got %+v", report)
	}
	if len(order) != 1 || order[0] != "n1" {
		t.Fatalf("expected n1 to run once, order=%v", order)
	}
	if report.Nodes["n2"].Status != workflow.StateSucceeded {
		t.Fatalf("n2 did not succeed: %+v", report.Nodes)
	}
}

func TestEngineSkipsDownstreamOnFailure(t *testing.T) {
	pool := workflow.NewPool()
	_ = pool.Register(workflow.AgentSpec{Name: "bad", Capability: workflow.Capability{
		Outputs: map[string]workflow.FieldSpec{"y": {Type: "string", Required: true}},
	}, Runner: runnerFunc(func(_ context.Context, _ workflow.NodeRequest) (workflow.NodeResult, error) {
		return workflow.NodeResult{Status: workflow.StateFailed, Error: "boom", Retryable: false}, nil
	})})
	_ = pool.Register(workflow.AgentSpec{Name: "next", Capability: workflow.Capability{
		Inputs: map[string]workflow.FieldSpec{"y": {Type: "string", Required: true}},
	}, Runner: runnerFunc(func(_ context.Context, _ workflow.NodeRequest) (workflow.NodeResult, error) {
		return workflow.NodeResult{Status: workflow.StateSucceeded}, nil
	})})
	w := workflow.Workflow{WorkflowID: "w", Nodes: []workflow.Node{
		{ID: "n1", Agent: "bad"},
		{ID: "n2", Agent: "next"},
	}, Edges: []workflow.Edge{{Src: "n1", Dst: "n2", Kind: workflow.EdgeData, Field: "y"}}}
	engine := workflow.NewEngine(pool, nil, nil, nil, workflow.EnginePolicy{MaxParallel: 2, RetryMax: 0, MaxNodesPerRun: 10}, nil)
	report, err := engine.Run(context.Background(), w, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != workflow.StateFailed {
		t.Fatalf("expected failed report, got %+v", report)
	}
	if report.Nodes["n1"].Status != workflow.StateFailed {
		t.Fatal("n1 should be failed")
	}
	// n2 未被执行，终态不是 SUCCEEDED
	if report.Nodes["n2"].Status == workflow.StateSucceeded {
		t.Fatal("下游不应在上游失败后成功")
	}
}

func TestEngineRetriesTransientFailure(t *testing.T) {
	attempts := 0
	pool := workflow.NewPool()
	_ = pool.Register(workflow.AgentSpec{Name: "flaky", Capability: workflow.Capability{
		Outputs: map[string]workflow.FieldSpec{"y": {Type: "string"}},
	}, Runner: runnerFunc(func(_ context.Context, _ workflow.NodeRequest) (workflow.NodeResult, error) {
		attempts++
		if attempts == 1 {
			return workflow.NodeResult{Status: workflow.StateFailed, Error: "transient", Retryable: true}, nil
		}
		return workflow.NodeResult{Status: workflow.StateSucceeded, Outputs: map[string]any{"y": "ok"}}, nil
	})})
	w := workflow.Workflow{WorkflowID: "w", Nodes: []workflow.Node{{ID: "n1", Agent: "flaky"}}}
	engine := workflow.NewEngine(pool, nil, nil, nil, workflow.EnginePolicy{MaxParallel: 1, RetryMax: 1, MaxNodesPerRun: 10, OnNodeFailure: "classify"}, nil)
	report, err := engine.Run(context.Background(), w, map[string]any{}, false)
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != workflow.StateSucceeded || attempts != 2 {
		t.Fatalf("expected retry success, attempts=%d report=%+v", attempts, report)
	}
}

func TestValidateFailsClosed(t *testing.T) {
	pool := workflow.NewPool()
	_ = pool.Register(workflow.AgentSpec{Name: "a", Capability: workflow.Capability{}, Runner: runnerFunc(func(context.Context, workflow.NodeRequest) (workflow.NodeResult, error) {
		return workflow.NodeResult{}, errors.New("should not run")
	})})
	w := workflow.Workflow{WorkflowID: "w", Nodes: []workflow.Node{{ID: "n", Agent: "missing"}}}
	engine := workflow.NewEngine(pool, nil, nil, nil, workflow.DefaultEnginePolicy(), nil)
	if _, err := engine.Run(context.Background(), w, map[string]any{}, false); err == nil {
		t.Fatal("校验失败时必须拒绝执行")
	}
}

func hasIssue(issues []workflow.GraphIssue, kind workflow.IssueKind) bool {
	for _, issue := range issues {
		if issue.Kind == kind {
			return true
		}
	}
	return false
}
