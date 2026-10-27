package composition

import (
	"github.com/besa-ai/besa-agent/internal/experience"
	"github.com/besa-ai/besa-agent/internal/store/postgres"
)

func BuildExperience(base *Base) (*experience.Manager, error) {
	cfg := base.Config
	var store experience.Store
	if base.DB != nil {
		store = &postgres.ExperienceRepo{DB: base.DB}
	} else {
		store = experience.NewMemoryStore()
	}
	gates := experience.GatePolicy{
		DedupeSimilarity:          cfg.Float("experience.distill.gates.dedupe_similarity", 0.9),
		RequireEvidence:           cfg.Bool("experience.distill.gates.require_evidence", true),
		RequireScope:              cfg.Bool("experience.distill.gates.require_scope", true),
		MinSuccessRate:            cfg.Float("experience.evaluate.min_success_rate", 0.6),
		MinSamples:                cfg.Int("experience.evaluate.min_samples", 3),
		RequireExpiryForVersioned: cfg.Bool("experience.distill.gates.require_expiry_for_versioned", true),
		DegradeAfterBadUses:       cfg.Int("experience.evaluate.degrade_after_bad_uses", 3),
	}
	return experience.NewManager(store, gates,
		cfg.Int("experience.recovery.max_retries", 1),
		cfg.Bool("experience.attribute.allow_hypothesis", false),
		base.Events), nil
}
