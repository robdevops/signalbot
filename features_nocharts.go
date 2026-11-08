//go:build nocharts

package main

import "github.com/robdevops/signalbot/internal/feature"

// chartsFeature is left out of this build (-tags nocharts), with its fonts.
var chartsFeature func(feature.Host) error
