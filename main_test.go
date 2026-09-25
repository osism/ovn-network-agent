package main

import (
	"log/slog"
	"testing"
)

func TestSetupLogging(t *testing.T) {
	for _, level := range []string{"debug", "info", "warn", "warning", "error", "unknown"} {
		t.Run(level, func(t *testing.T) {
			setupLogging(level)
		})
	}
}

func TestParseLogLevel(t *testing.T) {
	tests := map[string]slog.Level{
		"debug":   slog.LevelDebug,
		"info":    slog.LevelInfo,
		"warn":    slog.LevelWarn,
		"warning": slog.LevelWarn,
		"error":   slog.LevelError,
		"DEBUG":   slog.LevelDebug,
		"unknown": slog.LevelInfo,
		"":        slog.LevelInfo,
	}
	for in, want := range tests {
		if got := parseLogLevel(in); got != want {
			t.Errorf("parseLogLevel(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestVersionDefined(t *testing.T) {
	if version == "" {
		t.Error("version should not be empty")
	}
}
