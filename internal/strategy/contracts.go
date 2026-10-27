package strategy

// Contract 是纯函数：无 IO、无时间、无随机。
type Contract func(profile AgentProfile, registry *Registry) []Violation

func DefaultContracts() []Contract {
	return []Contract{
		readonlyBlocksWriteTools,
		stateCompressionLosesMemoryFields,
		crossValidationNeedsMultiagent,
		compareNeedsMultipleCandidates,
		statelessConflictsWithStructuredMemory,
	}
}

// Validate 不短路：一次报出全部违规。
func Validate(profile AgentProfile, registry *Registry, contracts []Contract) []Violation {
	var out []Violation
	for _, contract := range contracts {
		out = append(out, contract(profile, registry)...)
	}
	return out
}

func readonlyBlocksWriteTools(p AgentProfile, reg *Registry) []Violation {
	if p.StrategyOf(CompPermission) != "readonly" {
		return nil
	}
	selection := p.StrategyOf(CompToolSelection)
	if selection == "fixed" {
		return nil
	}
	// dependency_based / category 可能选中写工具。
	if selection == "dependency_based" || selection == "category" {
		return []Violation{{
			Rule: "readonly_blocks_write_tools", Components: []ComponentKind{CompPermission, CompToolSelection},
			Detail:     "permission=readonly 与 tool_selection=" + selection + " 组合后可能选中写工具",
			Suggestion: "把 permission 改为 restricted，或把 tool_selection 改成 fixed",
		}}
	}
	return nil
}

func stateCompressionLosesMemoryFields(p AgentProfile, _ *Registry) []Violation {
	if p.StrategyOf(CompContext) == "state_compression" && p.StrategyOf(CompMemory) != "stateless" {
		return []Violation{{
			Rule: "state_compression_loses_memory_fields", Components: []ComponentKind{CompContext, CompMemory},
			Detail:     "状态压缩上下文只保留任务状态，记忆组件需要的字段会缺失",
			Suggestion: "改用 tiered 上下文，或把 memory 设为 stateless",
		}}
	}
	return nil
}

func crossValidationNeedsMultiagent(p AgentProfile, reg *Registry) []Violation {
	if p.StrategyOf(CompValidation) != "cross_validation" {
		return nil
	}
	return []Violation{{
		Rule: "cross_validation_needs_multiagent", Components: []ComponentKind{CompValidation},
		Detail:     "validation.cross_validation 依赖 multiagent；当前组合没有提供多 agent 证据",
		Suggestion: "改用 validation.rules，或启用 multiagent 编排",
	}}
}

func compareNeedsMultipleCandidates(p AgentProfile, reg *Registry) []Violation {
	if p.StrategyOf(CompExecution) != "compare" {
		return nil
	}
	for _, kind := range ComponentKinds {
		name := p.StrategyOf(kind)
		if name == "" {
			continue
		}
		spec, err := reg.Get(kind, name)
		if err != nil {
			continue
		}
		for _, provided := range spec.Provides {
			if provided == "multiple_candidates" {
				return nil
			}
		}
	}
	return []Violation{{
		Rule: "compare_needs_multiple_candidates", Components: []ComponentKind{CompExecution},
		Detail:     "execution.compare 需要多个候选，但组合里没有人提供 multiple_candidates",
		Suggestion: "把 execution 改成 retry 或 single",
	}}
}

func statelessConflictsWithStructuredMemory(p AgentProfile, _ *Registry) []Violation {
	if p.StrategyOf(CompMemory) != "stateless" {
		return nil
	}
	if p.StrategyOf(CompRetrieval) == "case_kb" || p.StrategyOf(CompRetrieval) == "structured" {
		return []Violation{{
			Rule: "stateless_conflicts_with_structured_memory", Components: []ComponentKind{CompMemory, CompRetrieval},
			Detail:     "memory=stateless，但 retrieval 依赖记忆内容",
			Suggestion: "把 memory 改为 structured，或把 retrieval 改为 keyword/vector",
		}}
	}
	return nil
}
