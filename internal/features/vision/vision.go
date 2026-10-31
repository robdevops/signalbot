// Package vision shows the model the photos and stickers in a message, or
// in the message it quotes, when the model can see images; otherwise the
// model is told they're there and it can't see them. History keeps only a
// note, never the pictures, and says so, or the model concludes later that
// it can't see images at all.
package vision

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// notKept ends the history's note for an image the model saw.
const notKept = "; you see images only in the latest message, so to look again they must reply to it and mention you"

type Feature struct{}

func (Feature) Name() string { return "vision" }

func (Feature) Register(h feature.Host) error {
	h.OnTurn(func(ctx context.Context, t feature.Turn) feature.Additions { return add(ctx, h, t.Message) })
	return nil
}

// add fetches what the model may see, and writes the notes.
func add(ctx context.Context, h feature.Host, m chat.Message) feature.Additions {
	var images []chat.Media
	var sticker *chat.Media
	for _, md := range m.Media {
		switch {
		case md.Kind == chat.Sticker:
			sticker = &md
		case len(images) < img.MaxPerMessage:
			images = append(images, md)
		}
	}
	var a feature.Additions
	note := func(model, history string) {
		a.ModelNote = joinLines(a.ModelNote, model)
		a.HistoryNote = joinLines(a.HistoryNote, history)
	}
	if h.Caps().Vision {
		for _, md := range images {
			if url := fetch(ctx, h, m, md, "an image"); url != "" {
				a.Images = append(a.Images, url)
			}
		}
	}
	switch {
	case len(a.Images) > 0:
		note(imageNote(len(a.Images), "attached"), imageNote(len(a.Images), "attached"+notKept))
	case len(images) > 0:
		n := imageNote(len(images), "attached, which you can't see with this model")
		note(n, n)
	}
	if sticker != nil {
		url := ""
		if h.Caps().Vision {
			url = fetch(ctx, h, m, *sticker, "a sticker")
		}
		if url != "" {
			a.Images = append(a.Images, url)
			note("[sticker attached"+sticker.Label+"]", "[sticker attached"+sticker.Label+notKept+"]")
		} else {
			n := "[sticker attached" + sticker.Label + ", which you can't see]"
			note(n, n)
		}
	}
	return a
}

// fetch gets one picture as a data: URL for the model, or "" if it can't.
func fetch(ctx context.Context, h feature.Host, m chat.Message, md chat.Media, what string) string {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	data, err := h.Media().Fetch(ctx, m.Chat, md)
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
