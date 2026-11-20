package feature_test

import (
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
)

// The zero Action carries on; any one field set makes it act.
func TestActionIsZero(t *testing.T) {
	if !(feature.Action{}).IsZero() {
		t.Error("the zero Action isn't zero")
	}
	for name, a := range map[string]feature.Action{
		"Answer":     {Answer: "a"},
		"Pictures":   {Pictures: []feature.Picture{{}}},
		"OnSent":     {OnSent: func([]int64) {}},
		"ToolChoice": {ToolChoice: "t"},
		"Context":    {Context: "c"},
		"Done":       {Done: true},
	} {
		if a.IsZero() {
			t.Errorf("an Action with only %s set is zero", name)
		}
	}
}

// For keeps what applies to the message, and Done on both.
func TestActionFor(t *testing.T) {
	a := feature.Action{Answer: "a", Pictures: []feature.Picture{{}}, OnSent: func([]int64) {}, ToolChoice: "t", Context: "c", Done: true}
	if got := a.For(feature.Message{Addressed: true}); got.Answer != "a" || len(got.Pictures) != 1 || got.OnSent == nil || got.ToolChoice != "t" || got.Context != "" || !got.Done {
		t.Errorf("addressed: %+v", got)
	}
	if got := a.For(feature.Message{}); got.Answer != "" || got.Pictures != nil || got.OnSent != nil || got.ToolChoice != "" || got.Context != "c" || !got.Done {
		t.Errorf("unaddressed: %+v", got)
	}
	if got := (feature.Action{Answer: "a"}).For(feature.Message{}); !got.IsZero() {
		t.Errorf("an Answer counted on an unaddressed message: %+v", got)
	}
}
