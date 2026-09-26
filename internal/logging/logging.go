// Package logging builds the process logger. It redacts anything that looks
// like a credential before it reaches the output, as a backstop for code that
// should never log secrets in the first place.
package logging

import (
	"context"
	"io"
	"log/slog"
	"regexp"
	"strings"
)

// sensitiveKeys are attribute names whose values are always replaced.
var sensitiveKeys = []string{
	"password", "passwd", "authorization", "auth", "token", "secret", "cookie",
	"jwt", "signature", "verifier", "x_auth_password", "body",
}

// Patterns that look like credentials inside free-form strings.
var (
	jwtPattern    = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*`)
	bearerPattern = regexp.MustCompile(`(?i)(bearer|basic|oauth)\s+[A-Za-z0-9+/=._~,"-: ]{8,}`)
	appPwPattern  = regexp.MustCompile(`\b[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}\b`)
	kvPattern     = regexp.MustCompile(`(?i)((?:password|token|secret|oauth_[a-z_]+|x_auth_[a-z_]+)=)[^&\s]+`)
)

const redacted = "[REDACTED]"

// RedactString scrubs credential-looking substrings from s.
func RedactString(s string) string {
	s = jwtPattern.ReplaceAllString(s, redacted)
	s = bearerPattern.ReplaceAllString(s, "$1 "+redacted)
	s = appPwPattern.ReplaceAllString(s, redacted)
	s = kvPattern.ReplaceAllString(s, "${1}"+redacted)
	return s
}

func isSensitiveKey(k string) bool {
	k = strings.ToLower(k)
	for _, s := range sensitiveKeys {
		if k == s || strings.HasSuffix(k, "_"+s) || strings.HasSuffix(k, "."+s) {
			return true
		}
	}
	return false
}

// New returns a JSON logger writing to w at the given level.
func New(w io.Writer, level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lv, ReplaceAttr: replace})
	return slog.New(&redactHandler{h})
}

func replace(groups []string, a slog.Attr) slog.Attr {
	if isSensitiveKey(a.Key) {
		return slog.String(a.Key, redacted)
	}
	switch a.Value.Kind() {
	case slog.KindString:
		return slog.String(a.Key, RedactString(a.Value.String()))
	case slog.KindAny:
		if err, ok := a.Value.Any().(error); ok {
			return slog.String(a.Key, RedactString(err.Error()))
		}
	}
	return a
}

// redactHandler also scrubs the message itself.
type redactHandler struct{ slog.Handler }

func (h *redactHandler) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, RedactString(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool { nr.AddAttrs(a); return true })
	return h.Handler.Handle(ctx, nr)
}

func (h *redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &redactHandler{h.Handler.WithAttrs(as)}
}

func (h *redactHandler) WithGroup(name string) slog.Handler {
	return &redactHandler{h.Handler.WithGroup(name)}
}
