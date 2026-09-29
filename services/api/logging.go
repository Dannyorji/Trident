package main

import (
	"log/slog"
	"os"
)

// initLogger installs one process-wide slog configuration (Issue #456):
// JSON output in production (structured, machine-parseable for log
// aggregation), human-readable text in development. Call once at process
// startup, before any other package logs.
//
// This establishes the single logger configuration the issue asks for; it
// does not by itself guarantee every log line carries request id / route /
// key id / trace id — that requires threading a request-scoped logger (or
// slog attributes) through the existing middleware chain and switching
// remaining slog.Error calls to slog.ErrorContext, which is a larger,
// separate pass across every handler and left as follow-up.
func initLogger() {
	env := os.Getenv("APP_ENV")
	level := slog.LevelInfo
	if lvl := os.Getenv("LOG_LEVEL"); lvl != "" {
		if parsed, err := parseLogLevel(lvl); err == nil {
			level = parsed
		}
	}

	opts := &slog.HandlerOptions{Level: level}

	var handler slog.Handler
	if env == "production" {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		handler = slog.NewTextHandler(os.Stdout, opts)
	}

	// "service" is a base attribute on the logger itself, not something each
	// call site has to remember to add (issue #239) — so it appears on every
	// line, including ones StructuredLogging never sees (a slog.Error inside
	// a handler, a background job, a websocket debug log), not just the
	// per-request summary line.
	slog.SetDefault(slog.New(handler).With(slog.String("service", "trident-api")))
}

func parseLogLevel(s string) (slog.Level, error) {
	var level slog.Level
	err := level.UnmarshalText([]byte(s))
	return level, err
}
