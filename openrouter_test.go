package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Exactly one of the two keys must be set; each key selects its provider.
func TestProviderSelection(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		xai, openrouter, model  string
		wantErr                 bool
		wantProvider, wantModel string
		wantSearchWhenUnset     bool
	}{
		{name: "neither", wantErr: true},
		{name: "both", xai: "a", openrouter: "b", model: "x-ai/grok-4", wantErr: true},
		{name: "xai", xai: "a", wantProvider: "x.ai", wantModel: "grok-4.3", wantSearchWhenUnset: true},
		{name: "openrouter", openrouter: "b", wantProvider: "OpenRouter", wantModel: "deepseek/deepseek-v4.1-flash-1017", wantSearchWhenUnset: true},
		{name: "openrouter with a model", openrouter: "b", model: "x-ai/grok-4", wantProvider: "OpenRouter", wantModel: "x-ai/grok-4", wantSearchWhenUnset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SIGNALCLI_SOCKET", "/x")
			t.Setenv("XAI_API_KEY", tc.xai)
			t.Setenv("OPENROUTER_API_KEY", tc.openrouter)
			t.Setenv("MODEL", tc.model)
			t.Setenv("MODEL_SEARCH", "")
			t.Setenv("REASONING", "")
			c, err := LoadConfig()
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if c.Provider != tc.wantProvider || c.Model != tc.wantModel || c.Search != tc.wantSearchWhenUnset || c.Reasoning != "low" {
				t.Fatalf("got provider %q model %q search %v reasoning %q", c.Provider, c.Model, c.Search, c.Reasoning)
			}
		})
	}
}

func TestOpenRouterRequestAndReply(t *testing.T) {
	var got map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"choices":[{"message":{"content":" hi there "}}],` +
			`"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0042,"prompt_tokens_details":{"cached_tokens":40}}}`))
	}))
	defer srv.Close()

	cfg := &Config{Provider: "OpenRouter", APIKey: "or-key", Model: "x-ai/grok-4", Reasoning: "low", Search: true}
	o := NewOpenRouter(cfg)
	o.url = srv.URL
	res, err := o.Complete(context.Background(), "be brief", []ChatMsg{{Role: "user", Content: "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi there" || res.InputTokens != 100 || res.CachedTokens != 40 || res.OutputTokens != 20 {
		t.Fatalf("bad completion: %+v", res)
	}
	if res.Cost(cfg) != 0.0042 {
		t.Fatalf("the cost OpenRouter billed must be used as is, got %v", res.Cost(cfg))
	}
	if auth != "Bearer or-key" {
		t.Fatalf("Authorization = %q", auth)
	}
	if got["model"] != "x-ai/grok-4" {
		t.Fatalf("model = %v", got["model"])
	}
	msgs := got["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["role"] != "system" || msgs[1].(map[string]any)["content"] != "hello" {
		t.Fatalf("messages = %v", msgs)
	}
	if got["provider"].(map[string]any)["data_collection"] != "deny" {
		t.Fatalf("must opt out of data collection: %v", got["provider"])
	}
	if got["usage"].(map[string]any)["include"] != true {
		t.Fatalf("must ask for the cost: %v", got["usage"])
	}
	if got["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("reasoning = %v", got["reasoning"])
	}
	if got["tools"].([]any)[0].(map[string]any)["type"] != "openrouter:web_search" {
		t.Fatalf("search tool missing: %v", got["tools"])
	}
	if _, ok := got["plugins"]; ok {
		t.Fatal("the web plugin searches on every request; use the server tool the model chooses to call")
	}
	if _, ok := got["store"]; ok {
		t.Fatal("store is an x.ai field and must not be sent to OpenRouter")
	}
}

func TestOpenRouterOmitsOptionsWhenOff(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	cfg := &Config{Provider: "OpenRouter", APIKey: "k", Model: "m", Reasoning: "none", PriceIn: 1, PriceOut: 1}
	o := NewOpenRouter(cfg)
	o.url = srv.URL
	res, err := o.Complete(context.Background(), "s", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["reasoning"]; ok {
		t.Error("reasoning sent although REASONING=none")
	}
	if _, ok := got["tools"]; ok {
		t.Error("search tool sent although search is off")
	}
	if res.Billed != nil || res.Cost(cfg) <= 0 {
		t.Errorf("with no reported cost the token estimate must be used, got %v", res.Cost(cfg))
	}
}

func TestOpenRouterErrors(t *testing.T) {
	for name, body := range map[string]string{
		"no choices":    `{"choices":[],"usage":{}}`,
		"empty content": `{"choices":[{"message":{"content":"  "}}]}`,
		"not json":      `nope`,
	} {
		if _, err := parseChatCompletion([]byte(body)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"bad key"}`, http.StatusUnauthorized)
	}))
	defer srv.Close()
	o := NewOpenRouter(&Config{Provider: "OpenRouter", APIKey: "k", Model: "m"})
	o.url = srv.URL
	if _, err := o.Complete(context.Background(), "s", nil); err == nil {
		t.Fatal("a 401 must be an error")
	}
}
