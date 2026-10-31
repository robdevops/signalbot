// Package websearch lets the model search the web with OpenRouter's server
// tool, which it calls only when it decides to (unlike the deprecated "web"
// plugin, which searched on every request). MODEL_SEARCH turns it off.
package websearch

import (
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

type Feature struct{}

func (Feature) Name() string { return "websearch" }

func (Feature) Register(h feature.Host) error {
	if !h.Env().Bool("MODEL_SEARCH", true) || !h.Caps().ServerTools {
		return nil
	}
	h.Tools(feature.ToolGroup{Prompt: "web search", Intro: "web search", Tools: []feature.Tool{{
		Name: "web_search", Label: "web search", Def: map[string]string{"type": "openrouter:web_search"},
	}}})
	logx.Infof("web search on")
	return nil
}
