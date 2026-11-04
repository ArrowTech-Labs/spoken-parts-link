// Package logx is the link's only logger. Every line goes through a
// redact.Redactor before it is written.
package logx

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/redact"
)

// Level is a log level.
type Level int

// Levels, matching the add-on option log_level.
const (
	Debug Level = iota
	Info
	Warning
	Error
)

// ParseLevel maps "debug", "info", "warning" and "error" to a Level.
// Anything else is Info.
func ParseLevel(s string) Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return Debug
	case "warning", "warn":
		return Warning
	case "error":
		return Error
	default:
		return Info
	}
}

func (l Level) String() string {
	switch l {
	case Debug:
		return "DEBUG"
	case Warning:
		return "WARN"
	case Error:
		return "ERROR"
	default:
		return "INFO"
	}
}

// Logger writes redacted key=value lines.
type Logger struct {
	mu    sync.Mutex
	w     io.Writer
	red   *redact.Redactor
	level Level
	now   func() time.Time
}

// New returns a Logger writing to w through red.
func New(w io.Writer, red *redact.Redactor, level Level) *Logger {
	return &Logger{w: w, red: red, level: level, now: time.Now}
}

// Redactor returns the redactor used by this logger.
func (l *Logger) Redactor() *redact.Redactor { return l.red }

// Debug logs at debug level.
func (l *Logger) Debug(msg string, kv ...any) { l.log(Debug, msg, kv) }

// Info logs at info level.
func (l *Logger) Info(msg string, kv ...any) { l.log(Info, msg, kv) }

// Warn logs at warning level.
func (l *Logger) Warn(msg string, kv ...any) { l.log(Warning, msg, kv) }

// Error logs at error level.
func (l *Logger) Error(msg string, kv ...any) { l.log(Error, msg, kv) }

func (l *Logger) log(level Level, msg string, kv []any) {
	if l == nil || level < l.level {
		return
	}
	var b strings.Builder
	b.WriteString(l.now().UTC().Format(time.RFC3339))
	b.WriteByte(' ')
	b.WriteString(level.String())
	b.WriteByte(' ')
	b.WriteString(msg)
	for i := 0; i < len(kv); i += 2 {
		b.WriteByte(' ')
		key := fmt.Sprint(kv[i])
		b.WriteString(key)
		b.WriteByte('=')
		if i+1 < len(kv) {
			b.WriteString(formatValue(kv[i+1]))
		} else {
			b.WriteString("MISSING")
		}
	}
	l.write(b.String())
}

func formatValue(v any) string {
	var s string
	switch t := v.(type) {
	case error:
		if t == nil {
			return "nil"
		}
		s = t.Error()
	case string:
		s = t
	case fmt.Stringer:
		s = t.String()
	default:
		s = fmt.Sprint(v)
	}
	if s == "" || strings.ContainsAny(s, " \t\r\n\"=") {
		return strconv.Quote(s)
	}
	return s
}

func (l *Logger) write(line string) {
	line = l.red.Redact(line)
	line = strings.ReplaceAll(line, "\n", " ")
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = io.WriteString(l.w, line+"\n")
}

// Writer returns an io.Writer for libraries that want one (for example
// http.Server.ErrorLog). Each write is redacted and logged at the given level.
func (l *Logger) Writer(level Level) io.Writer {
	return writerFunc(func(p []byte) (int, error) {
		if level >= l.level {
			for _, line := range bytes.Split(bytes.TrimRight(p, "\n"), []byte("\n")) {
				l.log(level, string(line), nil)
			}
		}
		return len(p), nil
	})
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
