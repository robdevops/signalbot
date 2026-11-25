package bot

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
)

// stubNet is a network that owns the chats whose IDs start with its
// prefix, and records what it is asked; what it doesn't override, it
// doesn't do.
type stubNet struct {
	Network
	name, prefix string
	caps         feature.Caps
	id           chan string

	mu    sync.Mutex
	calls []string
}

func (n *stubNet) Owns(chat feature.Chat) bool {
	return strings.HasPrefix(chat.GroupID+chat.User, n.prefix)
}
func (n *stubNet) log(s string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, s)
}
func (n *stubNet) got() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return slices.Clone(n.calls)
}
func (n *stubNet) Name() string       { return n.name }
func (n *stubNet) BotName() string    { return n.name + "bot" }
func (n *stubNet) Caps() feature.Caps { return n.caps }
func (n *stubNet) Watch(Watch)        { n.log("watch") }
func (n *stubNet) Run(ctx context.Context, handle func(Received)) {
	n.log("run")
	<-ctx.Done()
}
func (n *stubNet) WaitID(ctx context.Context) string {
	select {
	case id := <-n.id:
		return id
	case <-ctx.Done():
		return ""
	}
}
func (n *stubNet) Send(chat feature.Chat, text string, _ []feature.Picture, _ map[string]string, _ int64, _ []byte, _ feature.Markup) []int64 {
	n.log("send " + text)
	return []int64{1}
}
func (n *stubNet) Groups(_ context.Context, user string) ([]feature.Group, error) {
	n.log("groups " + user)
	return []feature.Group{{ID: n.prefix + "g"}}, nil
}
func (n *stubNet) Redact(text string) string { return strings.ReplaceAll(text, n.prefix, "x") }

// Several networks act as one: a chat's calls go to the network that owns
// it, the first if none does; Run, Watch and WaitID go to all; Caps is
// what any can do.
func TestJoin(t *testing.T) {
	a := &stubNet{name: "a", prefix: "a", caps: feature.Caps{Caption: 10}, id: make(chan string, 1)}
	z := &stubNet{name: "z", prefix: "z", caps: feature.Caps{Buttons: true, Caption: 1024}, id: make(chan string, 1)}
	if Join(a) != Network(a) {
		t.Fatal("one network isn't itself")
	}
	j := Join(a, z)
	j.Send(feature.Chat{User: "z1"}, "to z", nil, nil, 0, nil, feature.Markup{})
	j.Send(feature.Chat{GroupID: "a1"}, "to a", nil, nil, 0, nil, feature.Markup{})
	j.Send(feature.Chat{User: "q1"}, "unowned", nil, nil, 0, nil, feature.Markup{})
	if g, _ := j.Groups(context.Background(), "z1"); len(g) != 1 || g[0].ID != "zg" {
		t.Errorf("groups %+v", g)
	}
	if !slices.Equal(a.got(), []string{"send to a", "send unowned"}) || !slices.Equal(z.got(), []string{"send to z", "groups z1"}) {
		t.Fatalf("a %q, z %q", a.got(), z.got())
	}
	if c := j.Caps(); !c.Buttons || c.Caption != 1024 || j.Name() != "a" || j.BotName() != "abot" {
		t.Errorf("caps %+v, name %q", c, j.Name())
	}
	if got := j.Redact("a-z"); got != "x-x" {
		t.Errorf("redact %q", got)
	}
	z.id <- "zid"
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if id := j.WaitID(ctx); id != "zid" {
		t.Errorf("WaitID %q", id)
	}
	j.Watch(Watch{})
	run, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { j.Run(run, nil); close(done) }()
	time.Sleep(20 * time.Millisecond)
	stop()
	<-done
	if !slices.Contains(a.got(), "run") || !slices.Contains(z.got(), "run") || !slices.Contains(z.got(), "watch") {
		t.Errorf("a %q, z %q", a.got(), z.got())
	}
	b := &Bot{net: j, cfg: &config.Config{Model: "p/m", Intro: "I am {name}"}}
	if n := b.Net(feature.Chat{User: "z9"}); n.Name != "z" || n.Bot != "zbot" || !n.Caps.Buttons || n.Intro != "I am zbot" {
		t.Errorf("Net %+v", n)
	}
}
