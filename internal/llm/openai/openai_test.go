package openai

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/llm"
)

const secret = "my-secret-message-text"

// openRouterAt is a client for a test server standing in for OpenRouter.
func openRouterAt(url string, opt Options) *Client {
	opt.URL = url
	c := New(opt)
	c.openRouter = true
	return c
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

	o := openRouterAt(srv.URL, Options{Key: "or-key", Model: "x-ai/grok-4", Reasoning: "low"})
	res, err := o.Chat(context.Background(), llm.Turn{Messages: []llm.Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hello"}},
		Tools: []any{map[string]string{"type": "openrouter:web_search"}}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "hi there" || res.InputTokens != 100 || res.CachedTokens != 40 || res.OutputTokens != 20 {
		t.Fatalf("bad completion: %+v", res)
	}
	if res.Cost != 0.0042 {
		t.Fatalf("the cost OpenRouter billed must be used as is, got %v", res.Cost)
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
}

func TestOpenRouterOmitsOptionsWhenOff(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	o := New(Options{URL: srv.URL, Key: "k", Model: "m", Reasoning: "none"})
	res, err := o.Chat(context.Background(), llm.Turn{Messages: []llm.Message{{Role: "system", Content: "s"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got["reasoning"]; ok {
		t.Error("reasoning sent although REASONING=none")
	}
	if _, ok := got["tools"]; ok {
		t.Errorf("tools sent though none were offered: %v", got["tools"])
	}
	if _, ok := got["provider"]; ok {
		t.Error("OpenRouter's provider routing sent to another provider")
	}
	if res.Cost != 0 {
		t.Errorf("with no reported cost, Cost must be 0, got %v", res.Cost)
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
	o := New(Options{URL: srv.URL, Key: "k", Model: "m"})
	if _, err := o.Chat(context.Background(), llm.Turn{}); err == nil {
		t.Fatal("a 401 must be an error")
	}
}

// The HTTP client must refuse servers that cannot do TLS 1.3, while keeping
// Go's default transport behaviour (proxy from the environment, HTTP/2).
func TestClientRequiresTLS13(t *testing.T) {
	x := New(Options{Model: "m"})
	tr := x.http.Transport.(*http.Transport)
	if tr.Proxy == nil || !tr.ForceAttemptHTTP2 {
		t.Fatal("lost the default transport's proxy support or HTTP/2")
	}
	for _, tc := range []struct {
		max  uint16
		want bool
	}{{tls.VersionTLS12, false}, {tls.VersionTLS13, true}} {
		srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		srv.TLS = &tls.Config{MaxVersion: tc.max}
		srv.StartTLS()
		tr.TLSClientConfig.RootCAs = x509.NewCertPool()
		tr.TLSClientConfig.RootCAs.AddCert(srv.Certificate())
		resp, err := x.http.Get(srv.URL)
		if resp != nil {
			resp.Body.Close()
		}
		srv.Close()
		if ok := err == nil; ok != tc.want {
			t.Fatalf("server capped at %x: connected=%v, want %v (err: %v)", tc.max, ok, tc.want, err)
		}
	}
}

// The startup check reads both images and tool calls from OpenRouter's
// model list. Unknown means no images but tools on, as before the check.
func TestCapabilities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[`+
			`{"id":"a/vision","architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools","reasoning"]},`+
			`{"id":"b/text","architecture":{"input_modalities":["text"]},"supported_parameters":["reasoning"]}]}`)
	}))
	defer srv.Close()
	for model, want := range map[string][2]bool{"a/vision": {true, true}, "b/text": {false, false}, "c/unknown": {false, true}} {
		o := New(Options{URL: srv.URL, Key: "k", Model: model})
		if images, tools := o.Capabilities(context.Background()); images != want[0] || tools != want[1] {
			t.Errorf("%s: images %v tools %v, want %v", model, images, tools, want)
		}
	}
	srv.Close()
	o := New(Options{URL: srv.URL, Key: "k", Model: "a/vision"})
	if images, tools := o.Capabilities(context.Background()); images || !tools {
		t.Errorf("a failed check: images %v tools %v, want false true", images, tools)
	}
}

func TestChatMsgWithImagesJSON(t *testing.T) {
	b, _ := json.Marshal(message{Role: "user", Content: "what's this?", Images: []string{"data:image/png;base64,AA=="}})
	want := `{"role":"user","content":[{"text":"what's this?","type":"text"},{"image_url":{"url":"data:image/png;base64,AA=="},"type":"image_url"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	if b, _ := json.Marshal(message{Role: "user", Content: "hi"}); string(b) != `{"role":"user","content":"hi"}` {
		t.Fatalf("plain message changed: %s", b)
	}
}

// API error bodies can quote the request, so only the message is logged.
func TestAPIErrorTextKeepsOnlyTheMessage(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"openai style": {`{"error":{"message":"No auth credentials","code":401}}`, "No auth credentials"},
		"openrouter moderation": {`{"error":{"message":"Input flagged","code":403,"metadata":{"flagged_input":"` +
			secret + `","reasons":["x"]}}}`, "Input flagged"},
		"html":  {`<html>502 Bad Gateway ` + secret + `</html>`, "(unreadable error body)"},
		"empty": {`{}`, "(no message)"},
	} {
		got := apiErrorText([]byte(tc.body))
		if got != tc.want || strings.Contains(got, secret) {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// A reply with no text, or one that is not JSON, must not echo the body: a
// response can include the conversation.
func TestBadReplyBodiesAreNotEchoed(t *testing.T) {
	for _, body := range []string{
		`{"output":[],"instructions":"` + secret + `"}`,
		`{"choices":[],"echo":"` + secret + `"}`,
		`{"output": ` + secret,
	} {
		if _, err := parseChatCompletion([]byte(body)); err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("body %q: err = %v", body, err)
		}
	}
}
