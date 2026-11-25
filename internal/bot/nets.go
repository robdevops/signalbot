package bot

import (
	"context"
	"sync"

	"github.com/robdevops/signalbot/internal/feature"
)

// Owner is a Network that can say whether a chat is its own, by the shape
// of its IDs, so a bot on several networks sends each chat's messages on
// the one it is on. A network that can't say owns no chat but its own
// messages' when it is one of several, and every chat when it is alone.
type Owner interface {
	Owns(chat feature.Chat) bool
}

// joined is several networks as one: each chat's calls go to the network
// that owns it, and Run, Watch and WaitID go to all of them. Its Name,
// BotName and Prompt are the first network's, and Caps what any of them
// can do: for a chat's own, ask the network that owns it (Bot.on).
type joined struct {
	all []Network
}

// Join is nets as one Network, for New: the network itself if there is
// one.
func Join(nets ...Network) Network {
	if len(nets) == 1 {
		return nets[0]
	}
	return &joined{all: nets}
}

// on is the network chat is on: the first that owns it, else the first.
func (j *joined) on(chat feature.Chat) Network {
	for _, n := range j.all {
		if o, ok := n.(Owner); ok && o.Owns(chat) {
			return n
		}
	}
	return j.all[0]
}

// Run runs every network until ctx ends, each handing its messages to
// handle, and returns once all have stopped.
func (j *joined) Run(ctx context.Context, handle func(Received)) {
	var wg sync.WaitGroup
	for _, n := range j.all {
		wg.Go(func() { n.Run(ctx, handle) })
	}
	wg.Wait()
}

func (j *joined) Watch(w Watch) {
	for _, n := range j.all {
		n.Watch(w)
	}
}

// WaitID waits for the first network to know the bot's ID, and returns
// it; "" if ctx ends first.
func (j *joined) WaitID(ctx context.Context) string {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	ids := make(chan string, len(j.all))
	for _, n := range j.all {
		go func() { ids <- n.WaitID(ctx) }()
	}
	for range j.all {
		if id := <-ids; id != "" {
			return id
		}
	}
	return ""
}

func (j *joined) Send(chat feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, mk feature.Markup) []int64 {
	return j.on(chat).Send(chat, text, pics, known, replyTo, audio, mk)
}

func (j *joined) Edit(chat feature.Chat, sent int64, text string, pics []feature.Picture, known map[string]string, mk feature.Markup) []int64 {
	return j.on(chat).Edit(chat, sent, text, pics, known, mk)
}

// Caps is what any of the networks can do: whether a feature needs to
// prepare for buttons at all. A chat's own are its network's.
func (j *joined) Caps() feature.Caps {
	var c feature.Caps
	for _, n := range j.all {
		nc := n.Caps()
		c.Buttons, c.Keyboard, c.Silent, c.EditMedia = c.Buttons || nc.Buttons, c.Keyboard || nc.Keyboard, c.Silent || nc.Silent, c.EditMedia || nc.EditMedia
		c.EditLimit, c.EditWindow, c.Caption = max(c.EditLimit, nc.EditLimit), max(c.EditWindow, nc.EditWindow), max(c.Caption, nc.Caption)
	}
	return c
}

func (j *joined) React(chat feature.Chat, msg feature.Message, emoji string) {
	j.on(chat).React(chat, msg, emoji)
}

func (j *joined) Typing(chat feature.Chat) (stop func()) { return j.on(chat).Typing(chat) }

func (j *joined) Delivered(ctx context.Context, msg feature.Message) (Ready, bool) {
	return j.on(msg.Chat).Delivered(ctx, msg)
}

func (j *joined) Reachable(ctx context.Context, chat feature.Chat) bool {
	return j.on(chat).Reachable(ctx, chat)
}

func (j *joined) Name() string    { return j.all[0].Name() }
func (j *joined) BotName() string { return j.all[0].BotName() }
func (j *joined) Prompt() string  { return j.all[0].Prompt() }

func (j *joined) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	return j.on(msg.Chat).Media(ctx, msg, i)
}

// Groups are the groups user shares with the bot, asked of the network
// user is on.
func (j *joined) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	return j.on(feature.Chat{User: user}).Groups(ctx, user)
}

// Redact hides what any of the networks would hide.
func (j *joined) Redact(text string) string {
	for _, n := range j.all {
		text = n.Redact(text)
	}
	return text
}

// on is the network chat is on.
func (b *Bot) on(chat feature.Chat) Network {
	if j, ok := b.net.(*joined); ok {
		return j.on(chat)
	}
	return b.net
}

// Net is what features know of chat's network.
func (b *Bot) Net(chat feature.Chat) feature.Net {
	n := b.on(chat)
	return feature.Net{Name: n.Name(), Bot: n.BotName(), Caps: n.Caps(), Intro: b.introOn(n, chat.GroupID == "")}
}

// networks are the names of the networks the bot is on, in order.
func (b *Bot) networks() []string {
	if j, ok := b.net.(*joined); ok {
		var names []string
		for _, n := range j.all {
			names = append(names, n.Name())
		}
		return names
	}
	return []string{b.net.Name()}
}
