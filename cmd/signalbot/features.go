package main

import (
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/features/charts"
	"github.com/robdevops/signalbot/internal/features/example"
	"github.com/robdevops/signalbot/internal/features/groupctx"
	"github.com/robdevops/signalbot/internal/features/mcp"
	"github.com/robdevops/signalbot/internal/features/schedule"
	"github.com/robdevops/signalbot/internal/features/vision"
	"github.com/robdevops/signalbot/internal/features/websearch"
)

// features register on the host in this order, which is the order their
// hooks run in and of the tools line and {tools}. One line per feature.
var features = []func(feature.Host) error{
	example.Register,
	websearch.Register,
	groupctx.Register,
	vision.Register,
	charts.Register, // after websearch, for the tools line
	mcp.Register,    // after websearch and charts, for the tools line
	schedule.Register,
}
