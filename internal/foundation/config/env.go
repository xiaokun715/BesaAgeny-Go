package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// interpolatePattern 匹配 ${VAR} 与 ${VAR:-default}。
var interpolatePattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)(?::-([^}]*))?\}`)

func mergeEnv(explicit map[string]string) map[string]string {
	out := map[string]string{}
	// 仓库根 .env 先读，进程环境优先覆盖（与 Python 版一致）。
	for _, dir := range []string{".", "..", filepath.Join("..", "..")} {
		if vals, err := readDotEnv(filepath.Join(dir, ".env")); err == nil {
			for k, v := range vals {
				out[k] = v
			}
			break
		}
	}
	for _, pair := range os.Environ() {
		if i := strings.IndexByte(pair, '='); i > 0 {
			out[pair[:i]] = pair[i+1:]
		}
	}
	for k, v := range explicit {
		out[k] = v
	}
	return out
}

func readDotEnv(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		i := strings.IndexByte(line, '=')
		if i <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		val = strings.Trim(val, `"'`)
		out[key] = val
	}
	return out, sc.Err()
}

func interpolate(v any, vars map[string]string) (any, error) {
	switch node := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(node))
		for k, child := range node {
			iv, err := interpolate(child, vars)
			if err != nil {
				return nil, err
			}
			out[k] = iv
		}
		return out, nil
	case []any:
		out := make([]any, len(node))
		for i, child := range node {
			iv, err := interpolate(child, vars)
			if err != nil {
				return nil, err
			}
			out[i] = iv
		}
		return out, nil
	case string:
		return interpolateString(node, vars)
	default:
		return v, nil
	}
}

func interpolateString(s string, vars map[string]string) (string, error) {
	var firstErr error
	out := interpolatePattern.ReplaceAllStringFunc(s, func(match string) string {
		m := interpolatePattern.FindStringSubmatch(match)
		name, def := m[1], m[2]
		if val, ok := vars[name]; ok && val != "" {
			return val
		}
		if def != "" || strings.Contains(match, ":-") {
			return def
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%w: 环境变量 %s 未定义且无默认值（出现在 %q）", ErrConfig, name, s)
		}
		return match
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}
