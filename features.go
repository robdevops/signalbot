package main

import (
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/features/files"
	"github.com/robdevops/signalbot/internal/features/groupctx"
	"github.com/robdevops/signalbot/internal/features/intro"
	"github.com/robdevops/signalbot/internal/features/links"
	"github.com/robdevops/signalbot/internal/features/mcp"
	"github.com/robdevops/signalbot/internal/features/schedule"
	"github.com/robdevops/signalbot/internal/features/vision"
	"github.com/robdevops/signalbot/internal/features/websearch"
)

// features register on the host in this order, which is the order their
// hooks run in and of the tools line and {tools}. One line per feature. A
// feature a build tag can leave out is a variable from its pair of files,
// features_<name>.go and features_no<name>.go, nil when left out; the order
// stays here.
var features = []func(feature.Host) error{
	intro.Register,
	websearch.Register,
	groupctx.Register,
	vision.Register,
	files.Register,
	links.Register,
	chartsFeature, // after websearch, for the tools line; build tag nocharts
	mcp.Register,  // after websearch and charts, for the tools line
	schedule.Register,
}

// enabled is features without those this build leaves out.
func enabled() []func(feature.Host) error {
	var out []func(feature.Host) error
	for _, f := range features {
		if f != nil {
			out = append(out, f)
		}
	}
	return out
}
