package config

import (
	"io"
	"log/slog"
)

// NewLogger returns a logger writing to w at the configured level and format.
func (c Config) NewLogger(w io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: c.SlogLevel()}
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
