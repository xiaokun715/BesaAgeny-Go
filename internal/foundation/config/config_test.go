package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/besa-ai/besa-agent/internal/foundation/config"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDeepMergeAndInterpolation(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "base.yaml", `
logging:
  level: INFO
  format: text
gateway:
  aliases:
    runtime.default:
      candidates: [m1]
      strategy: [capability, priority]
  retry:
    total_max_attempts: 4
providers:
  p:
    base_url: ${BASE_URL:-http://localhost:1}
    api_key_env: API_KEY
`)
	write(t, dir, "test.yaml", `
logging:
  level: WARNING
gateway:
  retry:
    total_max_attempts: 2
`)
	cfg, err := config.Load("test", dir, map[string]string{"BASE_URL": "http://example"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Env() != "test" {
		t.Fatal("env")
	}
	if cfg.String("logging.level", "") != "WARNING" {
		t.Fatalf("overlay not applied: %v", cfg.String("logging.level", ""))
	}
	if cfg.String("logging.format", "") != "text" {
		t.Fatal("base value lost in deep merge")
	}
	if cfg.Int("gateway.retry.total_max_attempts", 0) != 2 {
		t.Fatal("nested override failed")
	}
	if cfg.Int("gateway.retry.total_max_attempts", 0) == 4 {
		t.Fatal("nested override failed")
	}
	if cfg.String("providers.p.base_url", "") != "http://example" {
		t.Fatalf("interpolation failed: %q", cfg.String("providers.p.base_url", ""))
	}
	// 含点的 alias 键必须能读到
	if _, ok := cfg.Get("gateway.aliases.runtime.default"); !ok {
		t.Fatal("dotted alias key not addressable")
	}
	if got := cfg.StringSlice("gateway.aliases.runtime.default.candidates"); len(got) != 1 || got[0] != "m1" {
		t.Fatalf("candidates = %v", got)
	}
}

func TestMissingEnvFails(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "base.yaml", `x: ${MUST_DEFINE}\n`)
	if _, err := config.Load("dev", dir, nil); err == nil {
		t.Fatal("未定义且无默认值的变量必须报错")
	}
}
