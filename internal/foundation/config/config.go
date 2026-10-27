// Package config 负责配置加载、环境变量插值、深度合并与点分路径读取。
//
// 语义与 Python 版一致：
//   - base.yaml → <env>.yaml 深度合并，映射递归合并、列表整体替换；
//   - ${VAR} / ${VAR:-default} 在加载期插值，缺失且无默认值直接报错；
//   - 密钥只能以环境变量名的形式出现在配置里（providers.*.api_key_env）；
//   - Get 支持含点的键（如 runtime.default），按“最长前缀”贪心匹配。
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// ErrConfig 表示配置本身的错误（文件缺失、语法错误、插值变量未定义）。
var ErrConfig = errors.New("config error")

// Config 是一份已合并、已插值的配置快照。只读；并发安全。
type Config struct {
	env    string
	dir    string
	data   map[string]any
	envMap map[string]string
}

// Load 读取配置目录下的 base.yaml 与 <env>.yaml。
//
// env 为空时依次取 BESA_ENV、默认 dev。configDir 为空时依次取 BESA_CONFIG_DIR、
// ./configs、../configs（允许从子目录启动二进制）。
func Load(env, configDir string, envVars map[string]string) (*Config, error) {
	vars := mergeEnv(envVars)
	if env == "" {
		env = vars["BESA_ENV"]
	}
	if env == "" {
		env = "dev"
	}
	dir, err := resolveDir(configDir, vars)
	if err != nil {
		return nil, err
	}

	base, err := readYAML(filepath.Join(dir, "base.yaml"))
	if err != nil {
		return nil, err
	}
	merged := base
	overlayPath := filepath.Join(dir, env+".yaml")
	if _, statErr := os.Stat(overlayPath); statErr == nil {
		overlay, err := readYAML(overlayPath)
		if err != nil {
			return nil, err
		}
		merged = DeepMerge(merged, overlay)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: 读取 %s: %v", ErrConfig, overlayPath, statErr)
	}

	interpolated, err := interpolate(merged, vars)
	if err != nil {
		return nil, err
	}
	data, ok := interpolated.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: 配置根节点必须是映射", ErrConfig)
	}
	cfg := &Config{env: env, dir: dir, data: data, envMap: vars}
	cfg.applyEnvOverrides()
	return cfg, nil
}

func resolveDir(configDir string, vars map[string]string) (string, error) {
	candidates := []string{}
	if configDir != "" {
		candidates = append(candidates, configDir)
	} else if v := vars["BESA_CONFIG_DIR"]; v != "" {
		candidates = append(candidates, v)
	}
	candidates = append(candidates, "configs", filepath.Join("..", "configs"), filepath.Join("..", "..", "configs"))
	for _, dir := range candidates {
		if _, err := os.Stat(filepath.Join(dir, "base.yaml")); err == nil {
			abs, _ := filepath.Abs(dir)
			return abs, nil
		}
	}
	return "", fmt.Errorf("%w: 找不到 base.yaml（已尝试 %s）", ErrConfig, strings.Join(candidates, ", "))
}

func readYAML(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: 读取 %s: %v", ErrConfig, path, err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%w: 解析 %s: %v", ErrConfig, path, err)
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

// DeepMerge 递归合并映射；列表与其他标量整体替换（与 Python 版一致）。
func DeepMerge(base, overlay map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		if ov, ok := v.(map[string]any); ok {
			if bv, ok := out[k].(map[string]any); ok {
				out[k] = DeepMerge(bv, ov)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// ------------------------------ 读取 ------------------------------

func (c *Config) Env() string { return c.env }
func (c *Config) Dir() string { return c.dir }
func (c *Config) Data() map[string]any {
	return c.data
}
func (c *Config) EnvVars() map[string]string {
	out := make(map[string]string, len(c.envMap))
	for k, v := range c.envMap {
		out[k] = v
	}
	return out
}

// Get 按点分路径读取。键本身可能含点（runtime.default），因此先尝试最长前缀匹配。
func (c *Config) Get(path string) (any, bool) {
	path = strings.Trim(path, ".")
	if v, ok := lookupLongest(c.data, path); ok {
		return v, true
	}
	parts := strings.Split(path, ".")
	var cur any = c.data
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[p]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// lookupLongest 处理“键里含点”的情况：从整串开始，逐段退化。
func lookupLongest(m map[string]any, path string) (any, bool) {
	parts := strings.Split(path, ".")
	for end := len(parts); end >= 1; end-- {
		head := strings.Join(parts[:end], ".")
		v, ok := m[head]
		if !ok {
			continue
		}
		if end == len(parts) {
			return v, true
		}
		sub, ok := v.(map[string]any)
		if !ok {
			return nil, false
		}
		return lookupLongest(sub, strings.Join(parts[end:], "."))
	}
	return nil, false
}

// Section 取一个映射段；缺失或类型不符返回空映射（调用方按默认值处理）。
func (c *Config) Section(path string) map[string]any {
	v, ok := c.Get(path)
	if !ok {
		return map[string]any{}
	}
	m, ok := v.(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return m
}

// String 读取字符串；缺失时返回 def。
func (c *Config) String(path, def string) string {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func (c *Config) Int(path string, def int) int {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case string:
		if parsed, err := strconv.Atoi(n); err == nil {
			return parsed
		}
	}
	return def
}

func (c *Config) Float(path string, def float64) float64 {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case string:
		if parsed, err := strconv.ParseFloat(n, 64); err == nil {
			return parsed
		}
	}
	return def
}

func (c *Config) Bool(path string, def bool) bool {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return def
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		if parsed, err := strconv.ParseBool(b); err == nil {
			return parsed
		}
	}
	return def
}

func (c *Config) Duration(path string, def time.Duration) time.Duration {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return def
	}
	switch d := v.(type) {
	case float64:
		return time.Duration(d * float64(time.Second))
	case int:
		return time.Duration(d) * time.Second
	case string:
		if parsed, err := time.ParseDuration(d); err == nil {
			return parsed
		}
		if secs, err := strconv.ParseFloat(d, 64); err == nil {
			return time.Duration(secs * float64(time.Second))
		}
	}
	return def
}

// StringSlice 读取字符串列表；缺失返回 nil。
func (c *Config) StringSlice(path string) []string {
	v, ok := c.Get(path)
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		if s, ok := v.(string); ok {
			return []string{s}
		}
		return nil
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, fmt.Sprint(item))
	}
	return out
}

func (c *Config) IsSet(path string) bool {
	_, ok := c.Get(path)
	return ok
}

// IsNullLike：缺失或显式为 null。用于区分“没配”与“配了 null”。
func (c *Config) IsNullLike(path string) bool {
	v, ok := c.Get(path)
	return !ok || v == nil
}

func (c *Config) GetOrNil(path string) any {
	v, _ := c.Get(path)
	return v
}

// BoolFromAny 用于解析 capabilities 映射值。
func BoolFromAny(v any) bool {
	switch b := v.(type) {
	case bool:
		return b
	case string:
		parsed, _ := strconv.ParseBool(b)
		return parsed
	}
	return false
}

func (c *Config) applyEnvOverrides() {
	overrides := map[string][]string{
		"BESA_POSTGRES_DSN": {"postgres", "dsn"},
		"BESA_REDIS_URL":    {"redis", "url"},
		"BESA_LOG_LEVEL":    {"logging", "level"},
	}
	for envKey, path := range overrides {
		val := c.envMap[envKey]
		if val == "" {
			continue
		}
		setPath(c.data, path, val)
	}
}

func setPath(m map[string]any, path []string, val any) {
	cur := m
	for i, p := range path {
		if i == len(path)-1 {
			cur[p] = val
			return
		}
		next, ok := cur[p].(map[string]any)
		if !ok {
			next = map[string]any{}
			cur[p] = next
		}
		cur = next
	}
}
