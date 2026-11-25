package logx

import (
	"context"
	"strings"
)

// L is a log whose every line starts with its prefix, such as "Signal: ",
// the network it is about. The zero L has none.
type L struct{ prefix string }

// For is the log for the network called name ("signal"): its lines start
// with the name capitalised, "Signal: ".
func For(name string) L {
	if name == "" {
		return L{}
	}
	return L{prefix: strings.ToUpper(name[:1]) + name[1:] + ": "}
}

// Prefixed is format with l's prefix before it, for logf. A "%" in the
// prefix, which a network's name never has, is doubled all the same.
func (l L) prefixed(format string) string {
	return strings.ReplaceAll(l.prefix, "%", "%%") + format
}

func (l L) Critf(format string, args ...any)   { logf(2, l.prefixed(format), args...) }
func (l L) Errorf(format string, args ...any)  { logf(3, l.prefixed(format), args...) }
func (l L) Warnf(format string, args ...any)   { logf(4, l.prefixed(format), args...) }
func (l L) Noticef(format string, args ...any) { logf(5, l.prefixed(format), args...) }
func (l L) Infof(format string, args ...any)   { logf(6, l.prefixed(format), args...) }
func (l L) Debugf(format string, args ...any)  { logf(7, l.prefixed(format), args...) }

type ctxKey struct{}

// With is ctx carrying l, so whatever runs under it, the core's work for a
// chat and the features it calls, logs with l's prefix through Ctx.
func With(ctx context.Context, l L) context.Context { return context.WithValue(ctx, ctxKey{}, l) }

// Ctx is the log ctx carries (With), or the zero L, which adds nothing.
func Ctx(ctx context.Context) L {
	if ctx == nil {
		return L{}
	}
	l, _ := ctx.Value(ctxKey{}).(L)
	return l
}
