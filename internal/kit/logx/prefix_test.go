package logx

import (
	"context"
	"testing"
)

// A network's log, or one a context carries, starts each line with the
// network's name, capitalised; with none, nothing is added.
func TestPrefix(t *testing.T) {
	var buf Buffer
	t.Cleanup(SetOutput(&buf, &buf, true))
	For("signal").Infof("received dm")
	ctx := With(context.Background(), For("telegram"))
	Ctx(ctx).Warnf("%d%% done", 50)
	Ctx(context.Background()).Noticef("plain")
	var none context.Context
	Ctx(none).Infof("nil")
	For("").Infof("empty")
	want := "<6>Signal: received dm\n<4>Telegram: 50% done\n<5>plain\n<6>nil\n<6>empty\n"
	if got := buf.String(); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
