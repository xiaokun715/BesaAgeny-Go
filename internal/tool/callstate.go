package tool

import (
	"context"
	"fmt"
	"strings"
)

type callStateKey struct{}

// CallState 按**每次运行**隔离：不能放在 Executor 实例上，
// 否则并发运行会互相看见对方的调用栈，把合法调用误判成环。
type CallState struct {
	stack []string
	calls int
}

func WithCallState(ctx context.Context) context.Context {
	if _, ok := ctx.Value(callStateKey{}).(*CallState); ok {
		return ctx
	}
	return context.WithValue(ctx, callStateKey{}, &CallState{})
}

func stateFrom(ctx context.Context) *CallState {
	if ctx != nil {
		if s, ok := ctx.Value(callStateKey{}).(*CallState); ok {
			return s
		}
	}
	return &CallState{}
}

func (s *CallState) check(toolName string, maxDepth, maxCalls int) string {
	for _, name := range s.stack {
		if name == toolName {
			return fmt.Sprintf("检测到工具调用成环：%s → %s\n调用栈里已经出现过这个工具 —— 按这条路径走下去会无限循环。",
				strings.Join(s.stack, " → "), toolName)
		}
	}
	if maxDepth > 0 && len(s.stack) >= maxDepth {
		return fmt.Sprintf("工具调用深度已达上界（%d）：%s", maxDepth, strings.Join(s.stack, " → "))
	}
	if maxCalls > 0 && s.calls >= maxCalls {
		return fmt.Sprintf("本次运行的工具调用次数已达上界（%d）。这是给不可控增长加的上界，不是环检测。", maxCalls)
	}
	return ""
}

func (s *CallState) enter(toolName string) {
	s.calls++ // 只增不减：出栈不复位计数，计数按一次运行重置
	s.stack = append(s.stack, toolName)
}

func (s *CallState) exit() {
	if len(s.stack) > 0 {
		s.stack = s.stack[:len(s.stack)-1]
	}
}

func (s *CallState) Calls() int { return s.calls }
