package config

import (
	"io"
	"log/slog"
)

// NewLogger returns a logger writing to w at the configured level and format.
func (c Config) NewLogger(w io.Writer) *slog.Logger {
	return c.NewLoggerAt(w, c.SlogLevel())
}

// NewLoggerAt returns a logger writing to w in the configured format at
// level, which may be an *slog.LevelVar so the level can change while Hookd
// runs.
func (c Config) NewLoggerAt(w io.Writer, level slog.Leveler) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if c.LogFormat == LogFormatJSON {
		return slog.New(slog.NewJSONHandler(w, opts))
	}
	return slog.New(slog.NewTextHandler(w, opts))
}

// SlogLevel returns LogLevel as an slog.Level.
func (c Config) SlogLevel() slog.Level {
	switch c.LogLevel {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
