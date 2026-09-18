// Package logging builds the process logger. Logs are structured, carry no
// content, no tokens and no raw DIDs beyond a salted hash in access logs.
package logging

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New returns a slog.Logger writing JSON or text at the given level.
func New(level, format string, w io.Writer) *slog.Logger {
	if w == nil {
		w = os.Stderr
	}
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if format == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// HashDID returns a short salted hash of a DID for access logs.
func HashDID(salt, did string) string {
	if did == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(salt + "\x00" + did))
	return hex.EncodeToString(sum[:8])
}
