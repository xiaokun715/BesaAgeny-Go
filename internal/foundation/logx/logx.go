// Package logx 提供带 trace_id 的 slog 日志器。
package logx

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"github.com/besa-ai/besa-agent/internal/foundation/ids"
)

// New 构造 JSON 日志器（生产）或文本日志器（开发）。
func New(level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	if strings.EqualFold(format, "text") {
		h = slog.NewTextHandler(os.Stdout, opts)
	} else {
		h = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(&traceHandler{next: h})
}

type traceHandler struct{ next slog.Handler }

func (h *traceHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.next.Enabled(ctx, l)
}
func (h *traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := ids.Trace(ctx); id != "" {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.next.Handle(ctx, r)
}
func (h *traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceHandler{next: h.next.WithAttrs(attrs)}
}
func (h *traceHandler) WithGroup(name string) slog.Handler {
	return &traceHandler{next: h.next.WithGroup(name)}
}
