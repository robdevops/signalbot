// Package websearch lets the model search the web with OpenRouter's web
// search tool, when it decides to.
package websearch

import (
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// Register offers web search on h unless MODEL_SEARCH turns it off.
func Register(h feature.Host) error {
	if !txt.Bool(h.Env("MODEL_SEARCH", ""), true) {
		return nil
	}
	// A server tool the model calls only when it decides to search, unlike
	// the deprecated "web" plugin, which searched on every request.
	h.Tools(feature.ToolGroup{
		Names:   []string{"web search"},
		Listing: "web search",
		Tools:   []feature.Tool{{Server: "openrouter:web_search"}},
	})
	logx.Noticef("web search on")
	return nil
}
