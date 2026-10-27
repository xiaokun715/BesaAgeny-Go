package composition

import (
	"context"

	"github.com/besa-ai/besa-agent/internal/memory"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
)

func BuildMemory(base *Base, llm memory.LLM, embedder memory.Embedder) (*memory.Manager, error) {
	cfg := base.Config
	policy := memory.DefaultPolicy()
	policy.Enabled = cfg.Bool("memory.enabled", false)
	policy.MinChars = cfg.Int("memory.limits.min_chars", 4)
	policy.MaxChars = cfg.Int("memory.limits.max_chars", 2000)
	if scopes := cfg.StringSlice("memory.scope.allow"); len(scopes) > 0 {
		policy.AllowedScopes = nil
		for _, s := range scopes {
			policy.AllowedScopes = append(policy.AllowedScopes, memory.Scope(s))
		}
	}
	if kinds := cfg.StringSlice("memory.kinds.enabled"); len(kinds) > 0 {
		policy.EnabledKinds = nil
		for _, k := range kinds {
			policy.EnabledKinds = append(policy.EnabledKinds, memory.Kind(k))
		}
	}
	policy.RuleGate = cfg.Bool("memory.gates.rules", true)
	policy.LLMJudge = cfg.Bool("memory.gates.llm_judge.enabled", true)
	policy.JudgeOnUnavailable = cfg.String("memory.gates.llm_judge.on_unavailable", "reject")
	policy.PII = cfg.String("memory.gates.compliance.pii", "redact")
	policy.DenyKeywords = cfg.StringSlice("memory.gates.compliance.deny_keywords")
	policy.DenyPatterns = cfg.StringSlice("memory.gates.compliance.deny_patterns")
	policy.SimilarityThreshold = cfg.Float("memory.write.retrieve.similarity_threshold", 0.90)
	policy.MergeThreshold = cfg.Float("memory.write.retrieve.merge_threshold", 0.96)
	policy.TopK = cfg.Int("memory.retrieve.max_results", 20)
	policy.MaxBytes = cfg.Int("memory.retrieve.max_bytes", 32768)
	policy.LexicalWeight = cfg.Float("memory.retrieve.lexical_weight", 0.3)
	policy.Decay = memory.DecayPolicy{
		HalfLifeDays:          cfg.Float("memory.decay.half_life_days", 30),
		ProtectedHalfLifeDays: cfg.Float("memory.decay.protected_half_life_days", 365),
		TombstoneThreshold:    cfg.Float("memory.decay.tombstone_threshold", 0.05),
		ReinforceSaturate:     cfg.Float("memory.decay.reinforce_saturate", 5),
		EvidenceBoost:         cfg.Float("memory.decay.evidence_boost", 1),
	}

	var store memory.Store
	if base.DB != nil {
		repo := &postgres.MemoryRepo{
			DB:       base.DB,
			Space:    cfg.String("repo.embedding.space", "default"),
			Dim:      cfg.Int("repo.embedding.dim", 1024),
			ModelKey: cfg.String("repo.embedding.model_key", "emb-dashscope"),
		}
		if err := repo.EnsureSpace(context.Background()); err != nil {
			return nil, err
		}
		store = repo
	} else {
		store = memory.NewMemoryStore()
	}

	extractor := memory.NewExtractor(llm,
		cfg.Int("memory.write.extract.max_items_per_call", 5),
		cfg.Int("memory.write.extract.max_input_chars", 8000))
	return memory.NewManager(store, policy, extractor, llm, embedder, base.Events), nil
}
