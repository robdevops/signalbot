// Package websearch lets the model search the web with OpenRouter's web
// search tool, when it decides to.
package websearch

import (
	"slices"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// Register offers web search on h unless MODEL_SEARCH turns it off or there
// is no model. "web search on" is logged only when the host kept the tool:
// one whose API can't run it drops it and says so.
func Register(h feature.Host) error {
	if !txt.Bool(h.Env("MODEL_SEARCH", ""), true) || h.Info(feature.Chat{}).Model == "" {
		return nil
	}
	// A server tool the model calls only when it decides to search, unlike
	// the deprecated "web" plugin, which searched on every request.
	h.Tools(feature.ToolGroup{
		Names:   []string{"web search"},
		Listing: "web search",
		Tools:   []feature.Tool{{Server: "openrouter:web_search"}},
	})
	if info := h.Info(feature.Chat{}); info.CallsTools && !slices.Contains(info.Tools, "web search") {
		return nil
	}
	logx.Noticef("web search on")
	return nil
}
