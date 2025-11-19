// Package logging provides repo-wide slog helpers and format parsing.
package logging

import (
	"fmt"
	"log/slog"
	"os"
)

// New creates a slog.Logger configured with the desired level and format.
// format can be "json" or "console".
func New(level, format string) (*slog.Logger, error) {
	slogLevel, err := parseLevel(level)
	if err != nil {
		return nil, err
	}

	opts := &slog.HandlerOptions{
		Level: slogLevel,
	}

	handler, err := buildHandler(format, opts)
	if err != nil {
		return nil, err
	}

	return slog.New(handler), nil
}

func parseLevel(level string) (slog.Level, error) {
	switch level {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("invalid log level %q, must be one of: debug, info, warn, error", level)
	}
}

func buildHandler(format string, opts *slog.HandlerOptions) (slog.Handler, error) {
	switch format {
	case "json":
		return slog.NewJSONHandler(os.Stdout, opts), nil
	case "console":
		return slog.NewTextHandler(os.Stdout, opts), nil
	default:
		return nil, fmt.Errorf("invalid log format %q, must be 'json' or 'console'", format)
	}
}
