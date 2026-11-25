package bot

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// stubNet is a network that owns the chats whose IDs start with its
// prefix, and records what it is asked; what it doesn't override, it
// doesn't do.
type stubNet struct {
	Network
	name, prefix string
	caps         feature.Caps
	known        chan struct{} // closed once it knows its ID, idv
	idv          string

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
	case <-n.known:
		return n.idv
	case <-ctx.Done():
		return ""
	}
}
func (n *stubNet) Delivered(_ context.Context, msg feature.Message) (Ready, bool) {
	n.log("delivered " + msg.Text)
	return Ready{}, true
}
func (n *stubNet) Reachable(context.Context, feature.Chat) bool { return true }
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
	a := &stubNet{name: "a", prefix: "a", caps: feature.Caps{Caption: 10}, known: make(chan struct{})}
	z := &stubNet{name: "z", prefix: "z", caps: feature.Caps{Buttons: true, Caption: 1024}, known: make(chan struct{}), idv: "zid"}
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
	close(z.known)
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

// A post due before its chat's network is up waits for it, however long,
// rather than being dropped: the timers start once the first network is.
func TestJoinWaitsForTheChatsNetwork(t *testing.T) {
	a := &stubNet{name: "a", prefix: "a", known: make(chan struct{}), idv: "aid"}
	z := &stubNet{name: "z", prefix: "z", known: make(chan struct{}), idv: "zid"}
	close(z.known)
	j := Join(a, z)
	done := make(chan bool, 1)
	go func() {
		_, ok := j.Delivered(context.Background(), feature.Message{Chat: feature.Chat{User: "a1"}, Text: "due"})
		done <- ok && j.Reachable(context.Background(), feature.Chat{User: "a1"})
	}()
	select {
	case <-done:
		t.Fatal("delivered before its network was up")
	case <-time.After(50 * time.Millisecond):
	}
	close(a.known)
	if ok := <-done; !ok || !slices.Equal(a.got(), []string{"delivered due"}) {
		t.Fatalf("ok %v, a %q", ok, a.got())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b := &stubNet{name: "b", prefix: "b", known: make(chan struct{})}
	if _, ok := Join(b, z).Delivered(ctx, feature.Message{Chat: feature.Chat{User: "b1"}}); ok {
		t.Error("delivered while stopping")
	}
}

// answerNet is a network that records which message the bot answers.
type answerNet struct {
	*fakeNet
	mu    sync.Mutex
	calls []string
}

func (n *answerNet) Answering(chat feature.Chat, msg int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, fmt.Sprintf("answering %d", msg))
}

func (n *answerNet) Answered(feature.Chat) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, "answered")
}

// A network that asks is told which message the bot answers, before its
// reply goes out, and when it is done.
func TestAnswering(t *testing.T) {
	var an *answerNet
	h := start(t, options{wrap: func(n *fakeNet) Network { an = &answerNet{fakeNet: n}; return an }})
	m := dm("hello")
	m.Timestamp = 42
	h.receive(m)
	h.nextSend()
	h.b.Shutdown(5 * time.Second)
	an.mu.Lock()
	defer an.mu.Unlock()
	if len(an.calls) != 2 || an.calls[0] != fmt.Sprintf("answering %d", m.Timestamp) || an.calls[1] != "answered" {
		t.Fatalf("calls %q", an.calls)
	}
}

// The work for a chat logs as its network's: the core's lines and what the
// hooks and tools it runs log through their ctx, each chat on its own
// network's log.
func TestChatLog(t *testing.T) {
	ran := func(what string) func(context.Context) {
		return func(ctx context.Context) { logx.Ctx(ctx).Infof("%s ran", what) }
	}
	h := start(t, options{tools: true, features: func(b *Bot) {
		b.OnMessage(func(ctx context.Context, m feature.Message) feature.Action {
			ran("message hook")(ctx)
			return feature.Action{}
		})
		b.OnTurn(func(ctx context.Context, m feature.Message) feature.Additions {
			ran("turn hook")(ctx)
			return feature.Additions{}
		})
		b.OfferTools(feature.ToolGroup{Names: []string{"t"}, Tools: []feature.Tool{{Name: "draw", Run: func(ctx context.Context, _ string) (string, []feature.Picture, error) {
			ran("tool")(ctx)
			return "drawn", nil, nil
		}}}})
	}})
	h.answer(toolCall("draw"), text("Done"))
	h.receive(dm("draw"))
	h.nextSend()
	for _, want := range []string{"<6>Fake: received dm\n", "<6>Fake: message hook ran\n", "<6>Fake: turn hook ran\n", "<6>Fake: tool ran\n", "<6>Fake: tool draw ", "<6>Fake: reply "} {
		if !strings.Contains(h.logs.String(), want) {
			t.Errorf("no %q in the log:\n%s", want, h.logs)
		}
	}

	a := &stubNet{name: "alpha", prefix: "a"}
	z := &stubNet{name: "zulu", prefix: "z"}
	b := &Bot{net: Join(a, z), ctx: context.Background()}
	logs := &logx.Buffer{}
	defer logx.SetOutput(logs, logs, true)()
	logx.Ctx(b.chatCtx(feature.Chat{User: "z1"})).Infof("one")
	logx.Ctx(b.chatCtx(feature.Chat{GroupID: "a1"})).Infof("two")
	if got := logs.String(); got != "<6>Zulu: one\n<6>Alpha: two\n" {
		t.Fatalf("log %q", got)
	}
}
