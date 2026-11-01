// Package websearch lets the model search the web with OpenRouter's web
// search tool, when it decides to.
package websearch

import (
	"slices"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// label is the group's name in the intro's {tools}, in Markdown; the host
// lists it in Info.Tools only if it kept the tool.
const label = "**web search**"

// system is the group's system-prompt line while it is offered.
const system = "When you use web search, cite the sites you used by url."

// Register offers web search on h unless MODEL_SEARCH turns it off or there
// is no model. "web search on" is logged only when the host kept the tool:
// one whose API can't run it drops it and says so.
func Register(h feature.Host) error {
	if !txt.Bool(h.Env("MODEL_SEARCH", ""), true) || h.Info().Model == "" {
		return nil
	}
	// A server tool the model calls only when it decides to search, unlike
	// the deprecated "web" plugin, which searched on every request.
	h.Tools(feature.ToolGroup{
		Names:   []string{label},
		Listing: "web search",
		System:  system,
		Tools:   []feature.Tool{{Server: "openrouter:web_search"}},
	})
	if info := h.Info(); info.CallsTools && !slices.Contains(info.Tools, label) {
		return nil
	}
	logx.Noticef("web search on")
	return nil
}
