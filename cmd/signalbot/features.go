package main

import (
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/features/charts"
	"github.com/robdevops/signalbot/internal/features/example"
	"github.com/robdevops/signalbot/internal/features/groupctx"
	"github.com/robdevops/signalbot/internal/features/intro"
	"github.com/robdevops/signalbot/internal/features/mcp"
	"github.com/robdevops/signalbot/internal/features/vision"
	"github.com/robdevops/signalbot/internal/features/websearch"
)

// features is every feature, one line each, in the order their tools are
// named in the prompt and their hooks run. wait waits for those that need
// it at shutdown.
func features(cfg *config.Config) (fs []feature.Feature, wait func()) {
	tools := &mcp.Feature{Version: buildVersion(), Model: cfg.Model}
	return []feature.Feature{
		example.Feature{},
		websearch.Feature{},
		&charts.Feature{},
		tools,
		groupctx.Feature{},
		vision.Feature{},
		intro.Feature{},
	}, tools.Wait
}
