// Package vision shows the model the images and sticker a message carries,
// or tells it what it can't see, so a photo or sticker alone is answered.
package vision

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// notKept ends the history's note for an image the model saw. History keeps
// only the note, never the picture, and says so, or the model concludes
// later that it can't see images at all.
const notKept = "; you see images only in the latest message, so to look again they must reply to it and mention you"

// Register adds a message's images and sticker to the model's request on h.
func Register(h feature.Host) error {
	h.OnTurn(func(ctx context.Context, m feature.Message) feature.Additions { return additions(ctx, h, m) })
	return nil
}

// additions notes m's images and sticker, with the history's own version,
// and fetches them as data: URLs if the model can see images. One that
// can't be fetched is skipped.
func additions(ctx context.Context, h feature.Host, m feature.Message) feature.Additions {
	see := h.Info().Images
	var notes, kept, seen []string
	images, sticker := 0, -1
	for i, md := range m.Media {
		if md.Sticker {
			sticker = i
			continue
		}
		images++
		if see {
			if url := fetch(ctx, h, m, i, "an image"); url != "" {
				seen = append(seen, url)
			}
		}
	}
	switch {
	case len(seen) > 0:
		notes = append(notes, imageNote(len(seen), "attached"))
		kept = append(kept, imageNote(len(seen), "attached"+notKept))
	case images > 0:
		n := imageNote(images, "attached, which you can't see with this model")
		notes, kept = append(notes, n), append(kept, n)
	}
	if sticker >= 0 {
		label, url := m.Media[sticker].Label, ""
		if see {
			url = fetch(ctx, h, m, sticker, "a sticker")
		}
		if url != "" {
			notes = append(notes, "[sticker attached"+label+"]")
			kept = append(kept, "[sticker attached"+label+notKept+"]")
			seen = append(seen, url)
		} else {
			n := "[sticker attached" + label + ", which you can't see]"
			notes, kept = append(notes, n), append(kept, n)
		}
	}
	return feature.Additions{Note: strings.Join(notes, "\n"), HistoryNote: strings.Join(kept, "\n"), Images: seen}
}

// imageNote marks images in a prompt, e.g. "[1 image attached]".
func imageNote(n int, what string) string {
	if n == 1 {
		return "[1 image " + what + "]"
	}
	return fmt.Sprintf("[%d images %s]", n, what)
}

// fetch is m.Media[i] as a data: URL for the model, or "" if it can't be
// fetched or isn't an image the model takes.
func fetch(ctx context.Context, h feature.Host, m feature.Message, i int, what string) string {
	data, err := h.Media(ctx, m, i)
	if err == nil && !img.Sendable(data) {
		err = errors.New("not an image")
	}
	if err != nil {
		logx.Warnf("could not fetch %s: %v", what, err)
		return ""
	}
	return "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
}
