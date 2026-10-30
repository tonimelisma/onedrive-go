// Package loglevel defines the log levels the program adds to log/slog.
package loglevel

import "log/slog"

// Notice is for a record a person should see at default verbosity although
// nothing is wrong: chiefly the all-clear after a warning, such as OneDrive
// being reachable again. The default console shows WARN and above, so a
// recovery logged at INFO left a warned outage looking permanent. It sits
// between INFO and WARN, so the log file (INFO by default) keeps it too, and
// it maps to journald's own notice priority.
const Notice = slog.LevelInfo + 2

// noticeName is how Notice is spelled in log output.
const noticeName = "NOTICE"

// Name returns the name a handler should print for level: NOTICE for Notice,
// slog's own name for everything else.
func Name(level slog.Level) string {
	if level == Notice {
		return noticeName
	}

	return level.String()
}

// ReplaceLevelName is a slog.HandlerOptions.ReplaceAttr that prints NOTICE
// instead of slog's default "INFO+2".
func ReplaceLevelName(groups []string, attr slog.Attr) slog.Attr {
	if len(groups) != 0 || attr.Key != slog.LevelKey {
		return attr
	}
	level, ok := attr.Value.Any().(slog.Level)
	if !ok || level != Notice {
		return attr
	}

	return slog.String(slog.LevelKey, noticeName)
}
