package groupctx

import (
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// Unaddressed group messages are kept as "Name: text", with images and
// stickers noted; GROUP_CONTEXT=false turns it off.
func TestGroupContext(t *testing.T) {
	h := featuretest.New(nil)
	Feature{}.Register(h)
	h.Unaddressed[0](chat.Message{Chat: "g", Group: true, Sender: chat.Person{Name: "Alice"}, Text: "the moon",
		Media: []chat.Media{{Kind: chat.Image}, {Kind: chat.Sticker}}, Expires: time.Hour})
	h.Unaddressed[0](chat.Message{Chat: "g", Group: true}) // nothing to keep
	if len(h.Remembered) != 1 || h.Remembered[0].Text != "Alice: the moon [image] [sticker]" || h.Remembered[0].Expires != time.Hour {
		t.Fatalf("remembered %+v", h.Remembered)
	}
	off := featuretest.New(map[string]string{"GROUP_CONTEXT": "false"})
	Feature{}.Register(off)
	if len(off.Unaddressed) != 0 {
		t.Fatal("GROUP_CONTEXT=false still keeps chatter")
	}
}
