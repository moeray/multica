package lark

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
)

// Shared unit-test helpers for the lark package. (Formerly defined on
// hub_test.go, which the channel-engine cutover removed; relocated here so
// the surviving connector / outbound / runtime tests keep using them.)

func uuidFromString(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(s); err != nil {
		t.Fatalf("scan uuid %q: %v", s, err)
	}
	return u
}

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// capturedLog is one slog record reduced to what the tests assert on:
// its level, message, and rendered attributes.
type capturedLog struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// logCapture collects every record a logger emits so a test can prove a
// condition is reported exactly once (or not at all) — the MP-82
// "no repeated 41050 warning at every boot" contract.
type logCapture struct {
	mu      sync.Mutex
	records []capturedLog
}

// newLogCapture returns a logger that records instead of printing.
func newLogCapture() (*logCapture, *slog.Logger) {
	c := &logCapture{}
	return c, slog.New(captureHandler{c: c})
}

func (c *logCapture) all() []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedLog(nil), c.records...)
}

// countAtOrAbove counts records whose level is >= level AND whose message
// contains msgSubstr ("" matches every message).
func (c *logCapture) countAtOrAbove(level slog.Level, msgSubstr string) int {
	n := 0
	for _, r := range c.all() {
		if r.level >= level && strings.Contains(r.msg, msgSubstr) {
			n++
		}
	}
	return n
}

type captureHandler struct{ c *logCapture }

func (h captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h captureHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, r.NumAttrs())
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	h.c.records = append(h.c.records, capturedLog{level: r.Level, msg: r.Message, attrs: attrs})
	return nil
}

func (h captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h captureHandler) WithGroup(string) slog.Handler      { return h }
