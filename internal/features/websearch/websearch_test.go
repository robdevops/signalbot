package websearch

import (
	"reflect"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Web search is on by default: one always-offered group with OpenRouter's
// server tool, and a startup line saying so.
func TestWebSearchOnByDefault(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	h := featuretest.New()
	h.Bot = feature.Info{Model: "x/test-model", CallsTools: true, Tools: []string{label}}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	want := []feature.ToolGroup{{
		Names:   []string{label},
		Listing: "web search",
		System:  system,
		Tools:   []feature.Tool{{Server: "openrouter:web_search"}},
	}}
	if g := h.ToolGroups(); !reflect.DeepEqual(g, want) {
		t.Fatalf("groups %+v", g)
	}
	if !strings.Contains(out.String(), "<5>web search on\n") {
		t.Fatalf("log %q", out.String())
	}
	if r := h.EnvRead(); !reflect.DeepEqual(r, []string{"MODEL_SEARCH"}) {
		t.Fatalf("read %v", r)
	}
}

func TestWebSearchOff(t *testing.T) {
	for _, v := range []string{"false", "no", "off", "0"} {
		var out, errw logx.Buffer
		restore := logx.SetOutput(&out, &errw, true)
		h := featuretest.New()
		h.Bot = feature.Info{Model: "x/test-model"}
		h.Settings["MODEL_SEARCH"] = v
		if err := Register(h); err != nil {
			t.Fatal(err)
		}
		restore()
		if g := h.ToolGroups(); len(g) != 0 || out.String() != "" {
			t.Errorf("MODEL_SEARCH=%s: groups %+v, log %q", v, g, out.String())
		}
	}
}

// With no model, nothing is offered and nothing logged.
func TestWebSearchWithNoModel(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	h := featuretest.New()
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	if g := h.ToolGroups(); len(g) != 0 || out.String() != "" {
		t.Fatalf("groups %+v, log %q", g, out.String())
	}
}

// When the host drops the tool (another API), "web search on" isn't logged.
// A model that calls no tools lists none, and the line is logged as before.
func TestWebSearchDroppedByHost(t *testing.T) {
	for _, tc := range []struct {
		info feature.Info
		on   bool
	}{
		{feature.Info{Model: "x/test-model", CallsTools: true}, false},
		{feature.Info{Model: "x/test-model"}, true},
	} {
		var out, errw logx.Buffer
		restore := logx.SetOutput(&out, &errw, true)
		h := featuretest.New()
		h.Bot = tc.info
		if err := Register(h); err != nil {
			t.Fatal(err)
		}
		restore()
		if on := strings.Contains(out.String(), "web search on"); on != tc.on {
			t.Errorf("%+v: log %q", tc.info, out.String())
		}
	}
}
