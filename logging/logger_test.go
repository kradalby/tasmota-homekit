package logging

import (
	"log/slog"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name    string
		level   string
		format  string
		wantErr bool
		errMsg  string
	}{
		{name: "json debug", level: "debug", format: "json"},
		{name: "json info", level: "info", format: "json"},
		{name: "json warn", level: "warn", format: "json"},
		{name: "json error", level: "error", format: "json"},
		{name: "console info", level: "info", format: "console"},
		{name: "invalid level", level: "invalid", format: "json", wantErr: true, errMsg: "invalid log level"},
		{name: "invalid format", level: "info", format: "xml", wantErr: true, errMsg: "invalid log format"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, err := New(tt.level, tt.format)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error containing %q, got nil", tt.errMsg)
				}
				if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Fatalf("error = %v, want substring %q", err, tt.errMsg)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if logger == nil {
				t.Fatal("expected non-nil logger")
			}
		})
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name      string
		level     string
		wantLevel slog.Level
		wantErr   bool
	}{
		{name: "debug", level: "debug", wantLevel: slog.LevelDebug},
		{name: "info", level: "info", wantLevel: slog.LevelInfo},
		{name: "warn", level: "warn", wantLevel: slog.LevelWarn},
		{name: "error", level: "error", wantLevel: slog.LevelError},
		{name: "invalid", level: "fatal", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotLevel, err := parseLevel(tt.level)

			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for level %q", tt.level)
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if gotLevel != tt.wantLevel {
				t.Fatalf("parseLevel()=%v, want %v", gotLevel, tt.wantLevel)
			}
		})
	}
}

func TestLoggerOutput(t *testing.T) {
	logger, err := New("info", "json")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	logger.Info("info message")
	logger.Debug("debug message") // Should not be emitted but must not panic
	logger.Warn("warn message")
	logger.Error("error message")
}
