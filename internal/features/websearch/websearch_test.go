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
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	want := []feature.ToolGroup{{
		Names:   []string{"web search"},
		Listing: "web search",
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
