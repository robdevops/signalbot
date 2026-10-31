package bot

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// The model sees the photos and stickers in a message, or in the message
// it quotes, when it can see images; otherwise it is told they're there.
// History keeps only a note, never the pictures, and says so, or the model
// concludes later that it can't see images at all.

// notKept ends the history's note for an image the model saw.
const notKept = "; you see images only in the latest message, so to look again they must reply to it and mention you"

// see fetches the images and sticker in m that the model may see, and
// writes the notes for the model and for history.
func (b *Bot) see(ctx context.Context, m chat.Message) (modelNote, historyNote string, images []string) {
	var media []chat.Media
	var sticker *chat.Media
	for _, md := range m.Media {
		switch {
		case md.Kind == chat.Sticker:
			sticker = &md
		case len(media) < img.MaxPerMessage:
			media = append(media, md)
		}
	}
	note := func(model, history string) {
		modelNote = joinLines(modelNote, model)
		historyNote = joinLines(historyNote, history)
	}
	var seen []string
	if b.cfg.Caps.Vision {
		for _, md := range media {
			if url := b.fetch(ctx, m, md, "an image"); url != "" {
				seen = append(seen, url)
			}
		}
	}
	switch {
	case len(seen) > 0:
		note(imageNote(len(seen), "attached"), imageNote(len(seen), "attached"+notKept))
	case len(media) > 0:
		n := imageNote(len(media), "attached, which you can't see with this model")
		note(n, n)
	}
	if sticker != nil {
		url := ""
		if b.cfg.Caps.Vision {
			url = b.fetch(ctx, m, *sticker, "a sticker")
		}
		if url != "" {
			seen = append(seen, url)
			note("[sticker attached"+sticker.Label+"]", "[sticker attached"+sticker.Label+notKept+"]")
		} else {
			n := "[sticker attached" + sticker.Label + ", which you can't see]"
			note(n, n)
		}
	}
	return modelNote, historyNote, seen
}

// fetch gets one picture as a data: URL for the model, or "" if it can't.
func (b *Bot) fetch(ctx context.Context, m chat.Message, md chat.Media, what string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := b.tr(m.Chat).Fetch(ctx, m.Chat, md)
	if err != nil || !img.Sendable(data) {
		logx.Warnf("could not fetch %s from %s: %v", what, cmp.Or(m.Sender.Name, m.Sender.ID), cmp.Or(err, errors.New("not an image")))
		return ""
	}
	return "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// imageNote marks images in a prompt, e.g. "[1 image attached]".
func imageNote(n int, what string) string {
	if n == 1 {
		return "[1 image " + what + "]"
	}
	return fmt.Sprintf("[%d images %s]", n, what)
}

func joinLines(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}
