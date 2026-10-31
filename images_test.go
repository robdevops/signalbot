package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// dmPhoto is a photo with no text sent as a DM, in the shape signal-cli
// 0.14 sends it (ids made up).
const dmPhoto = `{"envelope":{"source":"` + alice + `","sourceNumber":null,"sourceUuid":"` + alice + `","sourceName":"Alice","sourceDevice":2,"timestamp":1,` +
	`"dataMessage":{"timestamp":1,"message":null,"expiresInSeconds":0,"isExpirationUpdate":false,"viewOnce":false,"attachments":[` +
	`{"contentType":"image/png","filename":null,"id":"photo1.png","size":34692,"width":534,"height":534,"caption":null,"isVoiceNote":false}]}},"account":"` + self + `"}`

// quotedPhoto is "@bot what's this?" in reply to someone else's photo: the
// quote carries only a thumbnail. The shape follows signal-cli's quote JSON.
var quotedPhoto = strings.Replace(groupMention, `"groupInfo"`,
	`"quote":{"id":1,"author":"`+bob+`","authorUuid":"`+bob+`","text":null,"attachments":[`+
		`{"contentType":"image/jpeg","filename":null,"thumbnail":{"contentType":"image/jpeg","id":"thumb1.jpg","size":5000}}]},"groupInfo"`, 1)

func TestPhotoTriggers(t *testing.T) {
	allowed := map[string]bool{alice: true}
	r := decide(parse(t, dmPhoto), allowed)
	if r == nil || r.Kind != "dm" || len(r.Images) != 1 || r.Images[0].ID != "photo1.png" || r.Text != "" {
		t.Fatalf("a DM photo with no text must be answered: %+v", r)
	}
	if !strings.HasSuffix(r.logLine(), " with 1 image") {
		t.Errorf("log line %q", r.logLine())
	}
	// Something that is not an image, with no text, is still ignored.
	voice := strings.Replace(dmPhoto, `"contentType":"image/png"`, `"contentType":"audio/aac"`, 1)
	if r := decide(parse(t, voice), allowed); r != nil {
		t.Fatalf("a voice note with no text was answered: %+v", r)
	}
	big := strings.Replace(dmPhoto, `"size":34692`, `"size":9000000`, 1)
	if r := decide(parse(t, big), allowed); r != nil {
		t.Fatalf("an oversized image was taken: %+v", r)
	}
	// A photo in a group that does not address the bot is ignored.
	group := strings.Replace(dmPhoto, `"attachments"`, `"groupInfo":{"groupId":"`+group+`"},"attachments"`, 1)
	if r := decide(parse(t, group), allowed); r != nil {
		t.Fatalf("an unaddressed group photo was answered: %+v", r)
	}
}

func TestQuotedPhotoIsSeen(t *testing.T) {
	r := decide(parse(t, quotedPhoto), nil)
	if r == nil || len(r.Images) != 1 || r.Images[0].ID != "thumb1.jpg" {
		t.Fatalf("the quoted photo's thumbnail was not taken: %+v", r)
	}
}

// recordingAI keeps the messages it was sent.
type recordingAI struct {
	mu   sync.Mutex
	msgs []ChatMsg
}

func (a *recordingAI) Complete(_ context.Context, _ string, msgs []ChatMsg) (Completion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgs = msgs
	return Completion{Text: "a cat"}, nil
}

// photoBot is a bot on a fake signal-cli that serves getAttachment, records
// its parameters, and answers sends.
func photoBot(t *testing.T, vision bool) (*Bot, *recordingAI, chan map[string]any, chan string) {
	t.Helper()
	params := make(chan map[string]any, 4)
	sent := make(chan string, 4)
	data, _ := base64.StdEncoding.DecodeString(tinyPNG())
	sock := fakeSignal(t, func(_ int, c net.Conn, r *bufio.Reader) {
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req map[string]any
			json.Unmarshal(line, &req)
			id, _ := json.Marshal(req["id"])
			switch req["method"] {
			case "listAccounts":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"number":null,"aci":"` + self + `"}],"id":` + string(id) + "}\n"))
			case "listContacts":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"uuid":"` + self + `","givenName":"Botty"}],"id":` + string(id) + "}\n"))
			case "getAttachment":
				params <- req["params"].(map[string]any)
				c.Write([]byte(`{"jsonrpc":"2.0","result":{"data":"` + base64.StdEncoding.EncodeToString(data) + `"},"id":` + string(id) + "}\n"))
			case "send":
				sent <- req["params"].(map[string]any)["message"].(string)
			}
		}
	})
	cfg := testCfg()
	cfg.AllowedSenders = map[string]bool{alice: true}
	sig := NewSignal(sock)
	ai := &recordingAI{}
	b := NewBot(context.Background(), cfg, sig, ai)
	b.vision = vision
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go sig.Run(ctx, b.Handle)
	sig.WaitSelf(5 * time.Second)
	return b, ai, params, sent
}

func waitSent(t *testing.T, sent chan string) {
	t.Helper()
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
}

// With a vision model, the photo is fetched over the socket and shown to the
// model; the history keeps only a marker; the log never holds the image.
func TestPhotoShownToModel(t *testing.T) {
	b, ai, params, sent := photoBot(t, true)
	logs := captureLogsSafely(t)
	b.Handle(parse(t, dmPhoto))
	waitSent(t, sent)
	if p := <-params; p["id"] != "photo1.png" || p["recipient"] != alice || p["groupId"] != nil {
		t.Fatalf("getAttachment params %v", p)
	}
	ai.mu.Lock()
	last := ai.msgs[len(ai.msgs)-1]
	ai.mu.Unlock()
	if last.Content != "[1 image attached]" || len(last.Images) != 1 || !strings.HasPrefix(last.Images[0], "data:image/png;base64,") {
		t.Fatalf("model got %q with %d images", last.Content, len(last.Images))
	}
	h := b.getHistory("u:" + alice)
	if len(h) != 2 || h[0].Content != "[1 image attached]" || h[0].Images != nil {
		t.Fatalf("history %+v", h)
	}
	if out := logs.String(); !strings.Contains(out, "with 1 image") || strings.Contains(out, "base64") || strings.Contains(out, "photo1") {
		t.Fatalf("log %q", out)
	}
}

// A quoted photo in a group is fetched by group id.
func TestQuotedPhotoFetchedByGroup(t *testing.T) {
	b, _, params, sent := photoBot(t, true)
	b.Handle(parse(t, quotedPhoto))
	waitSent(t, sent)
	if p := <-params; p["id"] != "thumb1.jpg" || p["groupId"] != group || p["recipient"] != nil {
		t.Fatalf("getAttachment params %v", p)
	}
}

// Without a vision model, nothing is fetched and the model is told it cannot
// see the image.
func TestPhotoWithoutVision(t *testing.T) {
	b, ai, params, sent := photoBot(t, false)
	b.Handle(parse(t, dmPhoto))
	waitSent(t, sent)
	if len(params) != 0 {
		t.Fatal("an image was fetched for a model that cannot see it")
	}
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if last := ai.msgs[len(ai.msgs)-1]; last.Content != "[1 image attached, which you can't see with this model]" || last.Images != nil {
		t.Fatalf("model got %q with %d images", last.Content, len(last.Images))
	}
}

func TestChatMsgWithImagesJSON(t *testing.T) {
	b, _ := json.Marshal(ChatMsg{Role: "user", Content: "what's this?", Images: []string{"data:image/png;base64,AA=="}})
	want := `{"role":"user","content":[{"text":"what's this?","type":"text"},{"image_url":{"url":"data:image/png;base64,AA=="},"type":"image_url"}]}`
	if string(b) != want {
		t.Fatalf("got  %s\nwant %s", b, want)
	}
	if b, _ := json.Marshal(ChatMsg{Role: "user", Content: "hi"}); string(b) != `{"role":"user","content":"hi"}` {
		t.Fatalf("plain message changed: %s", b)
	}
}

func TestSupportsImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"data":[{"id":"a/vision","architecture":{"input_modalities":["text","image"]}},{"id":"b/text","architecture":{"input_modalities":["text"]}}]}`)
	}))
	defer srv.Close()
	for model, want := range map[string]bool{"a/vision": true, "b/text": false, "c/unknown": false} {
		o := NewOpenRouter(&Config{APIKey: "k", Model: model}, nil)
		o.modelsURL = srv.URL
		if got := o.SupportsImages(context.Background()); got != want {
			t.Errorf("%s: got %v, want %v", model, got, want)
		}
	}
	srv.Close()
	o := NewOpenRouter(&Config{APIKey: "k", Model: "a/vision"}, nil)
	o.modelsURL = srv.URL
	if o.SupportsImages(context.Background()) {
		t.Error("a failed check must mean no images")
	}
}
