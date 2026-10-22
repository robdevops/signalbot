package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// logHandler is a slog.Handler that writes one "msg key=value ..." line per
// record, without the level text slog's own handlers always print.
//
// When stdout is the journal, each line starts with a syslog "<N>" priority,
// which journald strips and stores, so the level is kept as metadata:
// journalctl -p filters on it and highlights warnings and errors. A timestamp
// is added only when interactive; journald and Docker add their own.
type logHandler struct {
	mu        *sync.Mutex
	w         io.Writer
	timestamp bool
	priority  bool
	pre       []byte // attributes from WithAttrs, already formatted
	group     string // from WithGroup, dotted
}

func newLogHandler(w io.Writer, timestamp, priority bool) *logHandler {
	return &logHandler{mu: &sync.Mutex{}, w: w, timestamp: timestamp, priority: priority}
}

// setupLogging makes the bot's handler slog's default (and with it, the log
// package's), writing to w.
func setupLogging(w io.Writer, interactive, journal bool) {
	slog.SetDefault(slog.New(newLogHandler(w, interactive, journal)))
}

func (h *logHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= slog.LevelInfo }

func (h *logHandler) Handle(_ context.Context, r slog.Record) error {
	var b []byte
	if h.priority {
		b = append(b, '<', syslogPriority(r.Level), '>')
	}
	if h.timestamp && !r.Time.IsZero() {
		b = r.Time.AppendFormat(b, "2006/01/02 15:04:05 ")
	}
	// Messages are code constants, but quote one that would break the line.
	if hasControl(r.Message) {
		b = strconv.AppendQuote(b, r.Message)
	} else {
		b = append(b, r.Message...)
	}
	b = append(b, h.pre...)
	r.Attrs(func(a slog.Attr) bool {
		b = appendAttr(b, h.group, a)
		return true
	})
	b = append(b, '\n')
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := h.w.Write(b)
	return err
}

func (h *logHandler) WithAttrs(as []slog.Attr) slog.Handler {
	h2 := *h
	h2.pre = append([]byte(nil), h.pre...)
	for _, a := range as {
		h2.pre = appendAttr(h2.pre, h.group, a)
	}
	return &h2
}

func (h *logHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	h2 := *h
	if h2.group != "" {
		h2.group += "."
	}
	h2.group += name
	return &h2
}

// syslogPriority maps a slog level to the syslog severity digit journald
// reads from a "<N>" line prefix.
func syslogPriority(l slog.Level) byte {
	switch {
	case l >= slog.LevelError:
		return '3' // err
	case l >= slog.LevelWarn:
		return '4' // warning
	case l >= slog.LevelInfo:
		return '6' // info
	}
	return '7' // debug
}

// appendAttr writes " key=value", quoting the value as slog's TextHandler
// would: when it is empty or holds a space, '=', '"', or anything
// unprintable. So no value, however hostile, can start a new log line.
func appendAttr(b []byte, group string, a slog.Attr) []byte {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return b
	}
	key := a.Key
	if group != "" && key != "" {
		key = group + "." + key
	} else if key == "" {
		key = group
	}
	if a.Value.Kind() == slog.KindGroup {
		for _, ga := range a.Value.Group() {
			b = appendAttr(b, key, ga)
		}
		return b
	}
	b = append(b, ' ')
	b = append(b, key...)
	b = append(b, '=')
	var s string
	if a.Value.Kind() == slog.KindTime {
		s = a.Value.Time().Format(time.RFC3339)
	} else {
		s = a.Value.String()
	}
	if needsQuoting(s) {
		return strconv.AppendQuote(b, s)
	}
	return append(b, s...)
}

func needsQuoting(s string) bool {
	if s == "" {
		return true
	}
	for _, r := range s {
		if r == '=' || r == '"' || r == utf8.RuneError || unicode.IsSpace(r) || !unicode.IsPrint(r) {
			return true
		}
	}
	return false
}

func hasControl(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

// isTTY reports whether f is a terminal, i.e. we are running interactively
// rather than under systemd or Docker.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
