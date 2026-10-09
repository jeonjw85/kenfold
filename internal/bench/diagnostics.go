package bench

import (
	"context"
	"encoding/json"
	"log/slog"
)

// Only the structured retrieval event is forwarded. In particular, legacy
// warning messages and provider errors can contain URLs or credentials.
type benchDiagnosticHandler struct{ log func(string, ...any) }

func newBenchDiagnosticLogger(log func(string, ...any)) *slog.Logger {
	return slog.New(benchDiagnosticHandler{log: log})
}

func (h benchDiagnosticHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h benchDiagnosticHandler) Handle(_ context.Context, r slog.Record) error {
	if r.Message != "retrieval stage" || h.log == nil {
		return nil
	}
	fields := map[string]any{}
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "stage", "ids", "scores", "count", "elapsed_ms", "degraded":
			fields[a.Key] = a.Value.Any()
		}
		return true
	})
	b, err := json.Marshal(fields)
	if err == nil {
		h.log("retrieval %s", b)
	}
	return err
}
func (h benchDiagnosticHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h benchDiagnosticHandler) WithGroup(string) slog.Handler      { return h }
