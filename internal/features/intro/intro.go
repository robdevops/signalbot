// Package intro tells the model how to introduce itself: INTRO, with
// {name}, {model} and {tools} filled in.
package intro

import (
	"context"
	"strconv"
	"strings"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const readmeURL = "https://github.com/robdevops/signalbot/blob/main/README.md"

// Default is used when INTRO is unset.
const Default = "hi, I'm {name}, your open source assistant running {model} (tools: {tools}). " +
	"I respond to @mentions and replies to my messages. See " + readmeURL + " for details."

type Feature struct{}

func (Feature) Name() string { return "intro" }

func (Feature) Register(h feature.Host) error {
	intro := h.Env().String("INTRO", Default)
	h.OnTurn(func(_ context.Context, t feature.Turn) feature.Additions {
		info := h.Info()
		text := strings.NewReplacer("{name}", txt.Safe(t.Bot), "{model}", info.Model, "{tools}", info.Tools).Replace(intro)
		// Quoted so a newline or quote in the name or in INTRO cannot escape it.
		return feature.Additions{Prompt: "If asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(text)}
	})
	return nil
}
