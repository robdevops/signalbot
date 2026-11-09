//go:build !nocharts

package main

import "github.com/robdevops/signalbot/internal/features/charts"

// chartsFeature is in this build; -tags nocharts leaves it out.
var chartsFeature = charts.Register
