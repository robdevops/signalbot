package signal

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// Send splits the text at Signal's limit, turns "@Name" for names known in
// the chat into real mentions, and sends the pictures with the first part.
func (a *Adapter) Send(ctx context.Context, m chat.Outgoing) error {
	parts := txt.Split(m.Text, MaxChars)
	var attachments []string
	for i, p := range m.Pictures {
		attachments = append(attachments, dataURI(p.Data, fmt.Sprintf("image%d", i+1)))
	}
	if len(parts) == 0 {
		if len(attachments) == 0 {
			return nil
		}
		parts = []string{""} // the pictures alone
	}
	known, t := a.knownNames(m.Chat), target(m.Chat)
	for i, part := range parts {
		var att []string
		if i == 0 {
			att = attachments
		}
		if err := a.c.Send(t, part, buildMentions(part, known), att); err != nil {
			return err
		}
	}
	return nil
}

// dataURI is an image as signal-cli takes an attachment.
func dataURI(data []byte, name string) string {
	mime := http.DetectContentType(data)
	ext := strings.TrimPrefix(mime, "image/")
	return "data:" + mime + ";filename=" + name + "." + ext + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func (a *Adapter) Typing(c chat.ChatID) { a.c.Typing(target(c)) }

// Fetch downloads an image or sticker from a message in chat c.
func (a *Adapter) Fetch(ctx context.Context, c chat.ChatID, m chat.Media) ([]byte, error) {
	if m.Kind == chat.Sticker {
		pack, id, _ := strings.Cut(m.ID, ":")
		n, err := strconv.Atoi(id)
		if err != nil {
			return nil, err
		}
		return a.c.GetSticker(ctx, Sticker{PackID: pack, StickerID: n})
	}
	return a.c.GetAttachment(ctx, target(c), m.ID)
}

// Name is the bot's own profile name, loading the contact list once if it
// isn't known yet.
func (a *Adapter) Name(ctx context.Context) string {
	name := func() string { return a.names.lookup(a.c.SelfID()).name }
	if name() == "" && a.refreshContacts(ctx) && name() == "" {
		logx.Warnf("own profile name not found; calling itself signalbot (INTRO can name it)")
	}
	return name()
}

// Describe is the prompt text that sets the scene: a Signal chat, plain
// text, Signal's length limit, and how unknown mentions are shown.
func (a *Adapter) Describe() string {
	return "You are a helpful assistant in a Signal chat. " +
		"Reply in plain text: Signal does not render markdown. " +
		"Aim for a single line. Two or three sentences at the very most, and only when asked for detail. " +
		"Never exceed 2000 characters, which is Signal's limit for one message. " +
		"A mention shown as " + unnamedUser + " is someone whose name you don't know; don't invent one."
}

// Forget drops the names remembered for a chat: its history was erased, or
// its members changed. People still in it are remembered again by their
// next message.
func (a *Adapter) Forget(c chat.ChatID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.known, c)
}
