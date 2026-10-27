package composition

import (
	"context"

	ctxpkg "github.com/besa-ai/besa-agent/internal/context"
	"github.com/besa-ai/besa-agent/internal/memory"
)

// SummarizerAdapter 把 memory.LLM（带 schema）适配成 context.Summarizer（纯文本）。
type SummarizerAdapter struct {
	LLM memory.LLM
}

func (s SummarizerAdapter) Complete(ctx context.Context, system, prompt string, maxTokens int) (string, error) {
	return s.LLM.Complete(ctx, system, prompt, nil, maxTokens)
}

func BuildContext(base *Base, summarizer ctxpkg.Summarizer, memory ctxpkg.MemoryProvider, window func(string) *int) *ctxpkg.Manager {
	cfg := base.Config
	policy := ctxpkg.BudgetPolicy{
		ReserveOutput: cfg.Int("context.budget.reserve_output_tokens", 4096),
		SafetyMargin:  cfg.Int("context.budget.safety_margin_tokens", 1024),
		Ratios:        map[ctxpkg.Segment]float64{},
		Floors:        map[ctxpkg.Segment]int{},
	}
	if cfg.IsSet("context.budget.fallback_model_window") {
		fallback := cfg.Int("context.budget.fallback_model_window", 0)
		if fallback > 0 {
			policy.FallbackModelWindow = &fallback
		}
	} else {
		def := 32768
		policy.FallbackModelWindow = &def
	}
	for _, seg := range []ctxpkg.Segment{ctxpkg.SegmentWindow, ctxpkg.SegmentTransition, ctxpkg.SegmentSessionSummary, ctxpkg.SegmentLongTerm} {
		policy.Ratios[seg] = cfg.Float("context.budget.segments."+string(seg), defaultRatio(seg))
		policy.Floors[seg] = cfg.Int("context.budget.floors."+string(seg), defaultFloor(seg))
	}

	compactor := ctxpkg.NewCompactor(summarizer,
		cfg.String("context.linear.compact.max_level", "L2"),
		cfg.Int("context.linear.compact.l1_max_tool_output_chars", 2000),
		cfg.Int("context.linear.compact.max_llm_calls", 2),
		cfg.String("context.linear.compact.prompt_version", "v1"))

	managerCfg := ctxpkg.Config{
		Strategy:            cfg.String("context.strategy", "linear"),
		Alias:               cfg.String("context.alias", "runtime.default"),
		Dedupe:              cfg.Bool("context.dedupe", true),
		SettleOnClose:       cfg.Bool("context.settle_on_close", true),
		Budget:              policy,
		KeepRecent:          cfg.Int("context.linear.compact.keep_recent_turns", 8),
		MaxLevel:            cfg.String("context.linear.compact.max_level", "L2"),
		L1MaxToolChars:      cfg.Int("context.linear.compact.l1_max_tool_output_chars", 2000),
		MaxLLMCalls:         cfg.Int("context.linear.compact.max_llm_calls", 2),
		PromptVersion:       cfg.String("context.linear.compact.prompt_version", "v1"),
		TargetRatio:         cfg.Float("context.linear.compact.trigger_ratio", 0.75),
		WindowSize:          cfg.Int("context.tiered.window_size", 10),
		CompressEvery:       cfg.Int("context.tiered.compress_every", 6),
		TransitionMaxRounds: cfg.Int("context.tiered.transition_max_rounds", 20),
		LongTermTopK:        cfg.Int("context.tiered.long_term.top_k", 8),
		MemoryKinds:         cfg.StringSlice("context.tiered.long_term.kinds"),
		MemoryTopK:          cfg.Int("context.tiered.long_term.top_k", 8),
	}
	return ctxpkg.NewManager(managerCfg, compactor, memory, window, base.Events)
}

func defaultRatio(seg ctxpkg.Segment) float64 {
	return map[ctxpkg.Segment]float64{
		ctxpkg.SegmentWindow: 0.50, ctxpkg.SegmentTransition: 0.15,
		ctxpkg.SegmentSessionSummary: 0.20, ctxpkg.SegmentLongTerm: 0.15,
	}[seg]
}

func defaultFloor(seg ctxpkg.Segment) int {
	return map[ctxpkg.Segment]int{
		ctxpkg.SegmentWindow: 256, ctxpkg.SegmentTransition: 64,
		ctxpkg.SegmentSessionSummary: 64, ctxpkg.SegmentLongTerm: 64,
	}[seg]
}
