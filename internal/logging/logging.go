// Package logging configures the process-wide zerolog logger and hands out
// per-component sub-loggers.
//
// The global logger is installed in init() rather than from an explicit
// Setup() call in main(). This ordering is deliberate: packages tag their
// output with a file-level sub-logger created at package-init time, e.g.
//
//	var logger = logging.Component("sms")
//
// Go initializes an imported package before the importing package's
// variables, so any package that calls Component necessarily imports this
// package and is guaranteed to observe the fully configured global logger.
// If the global were configured later (from main), those sub-loggers would
// capture the unconfigured default — wrong writer, wrong level.
package logging

import (
	"os"
	"strings"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	// Level is info by default; DIESEL_LOG_LEVEL=debug surfaces the verbose
	// memory-pass / request traces that used to print unconditionally.
	level := zerolog.InfoLevel
	if v := strings.TrimSpace(os.Getenv("DIESEL_LOG_LEVEL")); v != "" {
		if parsed, err := zerolog.ParseLevel(strings.ToLower(v)); err == nil {
			level = parsed
		}
	}
	zerolog.SetGlobalLevel(level)

	cw := zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}
	log.Logger = zerolog.New(cw).With().Timestamp().Caller().Logger()
}

// Component returns a sub-logger whose lines carry component=<name>. Call it
// once at file scope; the returned logger inherits the global writer, level,
// timestamp, and caller settings.
func Component(name string) zerolog.Logger {
	return log.With().Str("component", name).Logger()
}
