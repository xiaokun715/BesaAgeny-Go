package gateway

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/besa-ai/besa-agent/internal/domain"
	"github.com/besa-ai/besa-agent/internal/foundation/config"
	"github.com/besa-ai/besa-agent/internal/provider"
	"github.com/besa-ai/besa-agent/internal/provider/mock"
)

// Factory 把“适配器名 + 模型配置”变成 provider 实例。
// 组合根负责注册；默认实现覆盖 openai / dashscope / vllm / mock。
type Factory func(adapter string, cfg provider.ModelConfig) provider.Provider

func DefaultFactory(adapter string, cfg provider.ModelConfig) provider.Provider {
	switch adapter {
	case "mock":
		return mock.New(cfg)
	default:
		return provider.NewCompatProvider(adapter, cfg, nil)
	}
}

var knownStrategies = map[string]bool{
	"capability": true, "priority": true, "weight": true, "cost": true, "health": true,
}

type Registry struct {
	mu        sync.RWMutex
	models    map[string]ModelSpec
	aliases   map[string]AliasSpec
	providers map[string]provider.Provider
	factory   Factory
}

type RegistryOptions struct {
	Factory Factory
	Env     map[string]string
}

// FromConfig 在**启动期做完全部校验**：厂商名拼错、候选悬空、策略名未知
// 都在这里硬失败；缺密钥只让该模型不可用（软失败）。
func FromConfig(cfg *config.Config, opts RegistryOptions) (*Registry, error) {
	factory := opts.Factory
	if factory == nil {
		factory = DefaultFactory
	}
	env := opts.Env
	if env == nil {
		env = cfg.EnvVars()
	}
	r := &Registry{
		models:    map[string]ModelSpec{},
		aliases:   map[string]AliasSpec{},
		providers: map[string]provider.Provider{},
		factory:   factory,
	}

	modelsCfg := cfg.Section("models")
	providersCfg := cfg.Section("providers")
	keys := sortedKeys(modelsCfg)
	if len(keys) == 0 {
		return nil, errInvalid("配置里没有任何 models")
	}
	for _, key := range keys {
		raw, ok := modelsCfg[key].(map[string]any)
		if !ok {
			return nil, errInvalid(fmt.Sprintf("models.%s 必须是映射", key))
		}
		spec, err := buildSpec(key, raw, providersCfg, env)
		if err != nil {
			return nil, err
		}
		r.models[key] = spec
	}

	aliasesCfg := cfg.Section("gateway.aliases")
	if len(aliasesCfg) == 0 {
		return nil, errInvalid("配置里没有任何 gateway.aliases")
	}
	for _, alias := range sortedKeys(aliasesCfg) {
		raw, _ := aliasesCfg[alias].(map[string]any)
		candidates := toStrings(raw["candidates"])
		if len(candidates) == 0 {
			return nil, errInvalid(fmt.Sprintf("gateway.aliases.%s 没有候选", alias))
		}
		for _, cand := range candidates {
			if _, ok := r.models[cand]; !ok {
				return nil, errInvalid(fmt.Sprintf("gateway.aliases.%s 引用了不存在的模型 %q", alias, cand))
			}
		}
		strategy := toStrings(raw["strategy"])
		if len(strategy) == 0 {
			strategy = []string{"capability", "priority"}
		}
		for _, s := range strategy {
			if !knownStrategies[s] {
				names := make([]string, 0, len(knownStrategies))
				for k := range knownStrategies {
					names = append(names, k)
				}
				sort.Strings(names)
				return nil, errInvalid(fmt.Sprintf("gateway.aliases.%s 使用未知策略 %q（可用：%s）", alias, s, strings.Join(names, ", ")))
			}
		}
		r.aliases[alias] = AliasSpec{Alias: alias, Candidates: candidates, Strategy: strategy}
	}
	return r, nil
}

// NewRegistry 供单元测试直接构造。
func NewRegistry(models map[string]ModelSpec, aliases map[string]AliasSpec, factory Factory) *Registry {
	if factory == nil {
		factory = DefaultFactory
	}
	return &Registry{models: models, aliases: aliases, providers: map[string]provider.Provider{}, factory: factory}
}

func buildSpec(key string, raw map[string]any, providersCfg map[string]any, env map[string]string) (ModelSpec, error) {
	adapter := str(raw["provider"])
	if adapter == "" {
		adapter = str(raw["type"])
	}
	if adapter == "" {
		adapter = "openai"
	}
	switch adapter {
	case "openai", "dashscope", "vllm", "mock":
	default:
		return ModelSpec{}, errInvalid(fmt.Sprintf("models.%s 使用未知厂商适配器 %q", key, adapter))
	}
	vendor := str(raw["vendor"])
	if vendor == "" {
		vendor = adapter
	}
	vendorCfg, _ := providersCfg[vendor].(map[string]any)
	baseURL := firstNonEmpty(str(raw["base_url"]), str(vendorCfg["base_url"]))
	if baseURL == "" && adapter != "mock" {
		baseURL = defaultBaseURL(adapter)
	}
	if baseURL == "" {
		baseURL = "mock://local"
	}

	capabilities := provider.ResolveCapabilities(raw["capabilities"], provider.DefaultCapabilities(adapter))
	if capabilityList, ok := raw["capabilities"].([]any); ok && len(capabilityList) == 0 {
		capabilities = domain.CapabilitySet{}
	}

	modelName := firstNonEmpty(str(raw["model"]), key)
	requireKey := adapter == "openai" || adapter == "dashscope"
	if v, ok := raw["require_api_key"].(bool); ok {
		requireKey = v
	}

	apiKeyEnv := firstNonEmpty(str(raw["api_key_env"]), str(vendorCfg["api_key_env"]))
	apiKey := firstNonEmpty(str(raw["api_key"]), envOr(env, apiKeyEnv))
	if apiKey == "" && requireKey && apiKeyEnv == "" {
		apiKey = envOr(env, defaultKeyEnv(adapter))
	}

	unavailable := ""
	if requireKey && apiKey == "" {
		envName := apiKeyEnv
		if envName == "" {
			envName = defaultKeyEnv(adapter)
		}
		unavailable = fmt.Sprintf("缺少密钥（%s 未设置）", envName)
	}

	timeout := time.Duration(sec(raw["timeout_s"], 120)) * time.Second
	backoff := time.Duration(float64(sec(raw["backoff_s"], 1.5)) * float64(time.Second))
	jitter := floatVal(raw["backoff_jitter"], 0.3)
	priority := intVal(raw["priority"], 0)
	weight := floatVal(raw["weight"], 1.0)

	spec := ModelSpec{
		Key:               key,
		Provider:          vendor,
		Model:             modelName,
		Capabilities:      capabilities,
		Priority:          priority,
		Weight:            weight,
		UnavailableReason: unavailable,
		Config: provider.ModelConfig{
			Key:                key,
			Adapter:            adapter,
			Vendor:             vendor,
			Model:              modelName,
			BaseURL:            baseURL,
			APIKey:             apiKey,
			APIKeyEnv:          apiKeyEnv,
			Temperature:        floatVal(raw["temperature"], 0.0),
			MaxTokens:          intVal(raw["max_tokens"], 2048),
			Timeout:            timeout,
			Retries:            intVal(raw["retries"], 1),
			Backoff:            backoff,
			BackoffJitter:      jitter,
			Capabilities:       capabilities,
			Dimension:          intVal(raw["dimension"], 0),
			MaxBatch:           intVal(raw["max_batch"], 0),
			SupportsJSONNative: boolVal(raw["supports_json_native"], true),
			UsesJSONObjectMode: boolVal(raw["uses_json_object_mode"], false),
			RequireAPIKey:      requireKey,
			Extra:              map[string]any{},
		},
	}
	if cw, ok := intPtr(raw["context_window"]); ok {
		if *cw <= 0 {
			return ModelSpec{}, errInvalid(fmt.Sprintf("models.%s.context_window 必须为正", key))
		}
		spec.ContextWindow = cw
	}
	return spec, nil
}

func defaultBaseURL(adapter string) string {
	switch adapter {
	case "openai":
		return "https://api.openai.com/v1"
	case "dashscope":
		return "https://dashscope.aliyuncs.com/compatible-mode/v1"
	case "vllm":
		return "http://127.0.0.1:8000/v1"
	}
	return ""
}

func defaultKeyEnv(adapter string) string {
	switch adapter {
	case "openai":
		return "OPENAI_API_KEY"
	case "dashscope":
		return "DASHSCOPE_API_KEY"
	}
	return ""
}

func envOr(env map[string]string, name string) string {
	if name == "" {
		return ""
	}
	return env[name]
}

// ------------------------------ 查询 ------------------------------

func (r *Registry) Alias(name string) (AliasSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.aliases[name]
	if !ok {
		avail := make([]string, 0, len(r.aliases))
		for k := range r.aliases {
			avail = append(avail, k)
		}
		sort.Strings(avail)
		return AliasSpec{}, &UnknownAliasError{Alias: name, Available: avail}
	}
	return a, nil
}

func (r *Registry) Candidates(alias string) ([]ModelSpec, error) {
	a, err := r.Alias(alias)
	if err != nil {
		return nil, err
	}
	out := make([]ModelSpec, 0, len(a.Candidates))
	for _, key := range a.Candidates {
		out = append(out, r.models[key])
	}
	return out, nil
}

func (r *Registry) ContextWindow(alias string) *int {
	a, err := r.Alias(alias)
	if err != nil {
		return nil
	}
	var best *int
	for _, key := range a.Candidates {
		spec := r.models[key]
		if spec.ContextWindow == nil {
			continue
		}
		if best == nil || *spec.ContextWindow < *best {
			best = spec.ContextWindow
		}
	}
	return best
}

func (r *Registry) Provider(modelKey string) (provider.Provider, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	spec, ok := r.models[modelKey]
	if !ok {
		return nil, errInvalid("未知模型键 " + modelKey)
	}
	if !spec.Available() {
		return nil, errInvalid(spec.UnavailableReason)
	}
	if p, ok := r.providers[modelKey]; ok {
		return p, nil
	}
	p := r.factory(spec.Config.Adapter, spec.Config)
	r.providers[modelKey] = p
	return p, nil
}

func (r *Registry) Models() []ModelSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ModelSpec, 0, len(r.models))
	for _, spec := range r.models {
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func (r *Registry) Aliases() []AliasSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AliasSpec, 0, len(r.aliases))
	for _, a := range r.aliases {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

func (r *Registry) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.providers {
		_ = p.Close()
	}
	r.providers = map[string]provider.Provider{}
	return nil
}

// ------------------------------ 小工具 ------------------------------

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func toStrings(v any) []string {
	switch list := v.(type) {
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case []string:
		return list
	case string:
		if list == "" {
			return nil
		}
		return []string{list}
	}
	return nil
}

func intPtr(v any) (*int, bool) {
	if v == nil {
		return nil, false
	}
	switch n := v.(type) {
	case int:
		return &n, true
	case int64:
		x := int(n)
		return &x, true
	case float64:
		x := int(n)
		return &x, true
	case string:
		var x int
		if _, err := fmt.Sscanf(n, "%d", &x); err == nil {
			return &x, true
		}
	}
	return nil, false
}

func intVal(v any, def int) int {
	if p, ok := intPtr(v); ok {
		return *p
	}
	return def
}

func floatVal(v any, def float64) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	}
	return def
}

func sec(v any, def float64) float64 { return floatVal(v, def) }

func boolVal(v any, def bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return def
}

var _ = os.Getenv
