package main

import "github.com/robdevops/signalbot/internal/feature"

// features register on the host in this order, which is the order their
// hooks run in and of the tools line and {tools}. One line per feature.
var features = []func(feature.Host) error{}
