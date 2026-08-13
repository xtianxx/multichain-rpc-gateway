// Package logging provides the gateway's JSON slog logger with double
// redaction (constitution V: payloads and secrets must never reach logs).
// Redact covers 0x-prefixed hex runs of 8+ chars (addresses, private keys,
// raw transaction payloads), bearer tokens, token= assignments, and URL
// userinfo credentials.
package logging

import (
	"io"
	"log/slog"
	"os"
	"regexp"
)

var (
	// Single greedy run: matches the full 0x-prefixed hex run (8+ chars) in
	// one replacement. An alternation would be wrong here — leftmost-first
	// matching would cut a 100-hex raw tx at 64 chars and leak the tail.
	hexRe      = regexp.MustCompile(`0x[0-9a-fA-F]{8,}`)
	bearerRe   = regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]+`)
	tokenRe    = regexp.MustCompile(`(?i)\b(token=)[^&\s]+`)
	userinfoRe = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s@]+@`)
)

// New builds the gateway's JSON slog logger at the given level, writing to
// stdout with redaction applied to every log record.
func New(level slog.Level) *slog.Logger {
	return NewWithOutput(os.Stdout, level)
}

// NewWithOutput is New with an explicit writer (used by tests and embedding).
func NewWithOutput(w io.Writer, level slog.Level) *slog.Logger {
	handler := slog.NewJSONHandler(w, &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: redactAttr,
	})
	return slog.New(handler)
}

// redactAttr is the second redaction layer: every string value in every
// record passes through Redact on its way to the handler.
func redactAttr(_ []string, a slog.Attr) slog.Attr {
	switch a.Value.Kind() {
	case slog.KindString:
		a.Value = slog.StringValue(Redact(a.Value.String()))
	case slog.KindAny:
		if s, ok := a.Value.Any().(string); ok {
			a.Value = slog.StringValue(Redact(s))
		}
	}
	return a
}

// Redact scrubs sensitive material from any string:
//   - 0x-prefixed hex runs of 8+ chars: addresses (40), private keys (64),
//     and longer raw transaction payloads (eth_sendRawTransaction); short
//     0x values below 8 hex chars pass through
//   - bearer tokens
//   - token=<value> query-style assignments
//   - URL userinfo credentials (scheme://user:pass@host)
//
// It is deterministic and never leaks the original value.
func Redact(s string) string {
	s = userinfoRe.ReplaceAllString(s, `${1}[REDACTED]@`)
	s = bearerRe.ReplaceAllString(s, `Bearer [REDACTED]`)
	s = tokenRe.ReplaceAllString(s, `${1}[REDACTED]`)
	s = hexRe.ReplaceAllString(s, `0x[REDACTED]`)
	return s
}
