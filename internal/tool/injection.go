package tool

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

type InjectionPolicy struct {
	CheckPathTraversal   bool
	URLAllowlist         []string
	BlockPrivateNetworks bool
	DeniedPaths          []string
}

func DefaultInjectionPolicy() InjectionPolicy {
	return InjectionPolicy{CheckPathTraversal: true, BlockPrivateNetworks: true}
}

type InjectionVerdict struct {
	Kind          string // path_traversal | url | denied_path | ""
	Detail        string
	Argument      string
	Authoritative bool
}

func (v InjectionVerdict) Safe() bool { return v.Kind == "" }

func (v InjectionVerdict) Reason() string {
	if v.Safe() {
		return ""
	}
	head := fmt.Sprintf("参数 %q 命中注入检测（%s）：%s", v.Argument, v.Kind, v.Detail)
	if !v.Authoritative {
		head += "\n（这一类是启发式检测，**不是安全边界**）"
	}
	return head
}

// Check 只扫描顶层字符串与字符串列表；不递归映射。
func (p InjectionPolicy) Check(args map[string]any, allowedPaths []string) InjectionVerdict {
	keys := sortedStringKeys(args)
	for _, key := range keys {
		values := stringValues(args[key])
		for _, value := range values {
			if strings.Contains(value, "://") {
				if v := p.checkURL(key, value); !v.Safe() {
					return v
				}
			}
			if p.CheckPathTraversal && looksLikePath(key) {
				if v := p.checkPath(key, value, allowedPaths); !v.Safe() {
					return v
				}
			}
		}
	}
	return InjectionVerdict{}
}

func (p InjectionPolicy) checkURL(argument, raw string) InjectionVerdict {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return InjectionVerdict{Kind: "url", Argument: argument, Detail: "只允许 http/https 地址", Authoritative: true}
	}
	host := u.Hostname()
	if host == "" {
		return InjectionVerdict{Kind: "url", Argument: argument, Detail: "缺少主机名", Authoritative: true}
	}
	lower := strings.ToLower(host)
	if lower == "localhost" || strings.HasSuffix(lower, ".local") || strings.HasSuffix(lower, ".internal") ||
		lower == "metadata" || lower == "metadata.google.internal" || lower == "instance-data" {
		return InjectionVerdict{Kind: "url", Argument: argument, Detail: "拒绝本地/元数据主机 " + host, Authoritative: true}
	}
	if p.BlockPrivateNetworks {
		if ip := net.ParseIP(host); ip != nil && isPrivateIP(ip) {
			return InjectionVerdict{Kind: "url", Argument: argument, Detail: "拒绝内网/保留地址 " + host, Authoritative: true}
		}
	}
	if len(p.URLAllowlist) == 0 {
		return InjectionVerdict{Kind: "url", Argument: argument, Detail: "URL 白名单为空 = 不允许任何外部地址", Authoritative: true}
	}
	for _, allowed := range p.URLAllowlist {
		if host == allowed || strings.HasSuffix(host, "."+allowed) {
			return InjectionVerdict{}
		}
	}
	return InjectionVerdict{Kind: "url", Argument: argument, Detail: fmt.Sprintf("主机 %s 不在白名单（%s）", host, strings.Join(p.URLAllowlist, ", ")), Authoritative: true}
}

func (p InjectionPolicy) checkPath(argument, raw string, allowedPaths []string) InjectionVerdict {
	if raw == "" {
		return InjectionVerdict{}
	}
	var target string
	if filepath.IsAbs(raw) {
		target = ResolvePath(raw)
	} else if len(allowedPaths) > 0 {
		target = ResolvePath(filepath.Join(allowedPaths[0], raw))
	} else {
		target = ResolvePath(raw)
	}
	for _, denied := range p.DeniedPaths {
		if matchDenied(target, denied) {
			return InjectionVerdict{Kind: "denied_path", Argument: argument, Detail: "命中敏感路径 " + denied, Authoritative: true}
		}
	}
	if len(allowedPaths) == 0 {
		return InjectionVerdict{Kind: "path_traversal", Argument: argument, Detail: "没有配置任何可访问范围", Authoritative: true}
	}
	ctx := &Context{AllowedPaths: allowedPaths}
	if !ctx.Within(target) {
		return InjectionVerdict{Kind: "path_traversal", Argument: argument, Detail: fmt.Sprintf("解析后超出授权范围：%s", target), Authoritative: true}
	}
	return InjectionVerdict{}
}

func matchDenied(target, pattern string) bool {
	base := filepath.Base(target)
	if ok, _ := filepath.Match(pattern, base); ok {
		return true
	}
	if strings.HasPrefix(pattern, "**/") {
		tail := strings.TrimPrefix(pattern, "**/")
		if ok, _ := filepath.Match(tail, base); ok {
			return true
		}
	}
	if ok, _ := filepath.Match(pattern, target); ok {
		return true
	}
	return false
}

func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// 云元数据地址
	if ip.Equal(net.ParseIP("169.254.169.254")) {
		return true
	}
	return false
}

func looksLikePath(argument string) bool {
	lower := strings.ToLower(argument)
	for _, hint := range []string{"path", "file", "dir", "cwd", "target", "output", "source"} {
		if strings.Contains(lower, hint) {
			return true
		}
	}
	return false
}

func stringValues(v any) []string {
	switch val := v.(type) {
	case string:
		return []string{val}
	case []any:
		out := make([]string, 0, len(val))
		for _, item := range val {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return val
	}
	return nil
}

func sortedStringKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	// 稳定遍历：先 URL，再路径；按 key 排序足够。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
