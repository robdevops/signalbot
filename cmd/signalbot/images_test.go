package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/llm"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/transports/signal"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
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
	msgs []llm.Message
}

func (a *recordingAI) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.msgs = r.Messages
	return llm.Completion{Text: "a cat"}, nil
}

// photoBot is a bot on a fake signal-cli that serves getAttachment, records
// its parameters, and answers sends.
func photoBot(t *testing.T, vision bool, opts ...func(*Config)) (*bot.Bot, *recordingAI, chan map[string]any, chan string) {
	t.Helper()
	params := make(chan map[string]any, 4)
	sent := make(chan string, 4)
	data, _ := base64.StdEncoding.DecodeString(tinyPNG())
	sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
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
			case "listStickerPacks":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"packId":"other","title":"Dogs","stickers":[]},` +
					`{"packId":"00aabbccddeeff00","title":"My Daily Life","stickers":[{"id":1,"emoji":"❤️"},{"id":2,"emoji":"🕵️"}]}],"id":` + string(id) + "}\n"))
			case "getSticker":
				params <- req["params"].(map[string]any)
				webp := base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00"))
				c.Write([]byte(`{"jsonrpc":"2.0","result":{"data":"` + webp + `"},"id":` + string(id) + "}\n"))
			case "send":
				sent <- req["params"].(map[string]any)["message"].(string)
			}
		}
	})
	cfg := testCfg()
	cfg.AllowedSenders = map[string]bool{alice: true}
	cfg.Vision = vision
	for _, o := range opts {
		o(cfg)
	}
	sig := signal.NewAdapter(sock, cfg.AllowedSenders)
	ai := &recordingAI{}
	b := NewBot(context.Background(), cfg, ai, sig)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Run(ctx)
	sig.Client().WaitSelf(5 * time.Second)
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
	logs := logxtest.Capture(t)
	deliver(b, parse(t, dmPhoto))
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
	h := b.History(chat.ChatID("signal:u:" + alice))
	if len(h) != 2 || h[0].Content != "[1 image attached; you see images only in the latest message, so to look again they must reply to it and mention you]" {
		t.Fatalf("history %+v", h)
	}
	if out := logs.String(); !strings.Contains(out, "with 1 image") || strings.Contains(out, "base64") || strings.Contains(out, "photo1") {
		t.Fatalf("log %q", out)
	}
}

// A quoted photo in a group is fetched by group id.
func TestQuotedPhotoFetchedByGroup(t *testing.T) {
	b, _, params, sent := photoBot(t, true)
	deliver(b, parse(t, quotedPhoto))
	waitSent(t, sent)
	if p := <-params; p["id"] != "thumb1.jpg" || p["groupId"] != group || p["recipient"] != nil {
		t.Fatalf("getAttachment params %v", p)
	}
}

// Without a vision model, nothing is fetched and the model is told it cannot
// see the image.
func TestPhotoWithoutVision(t *testing.T) {
	b, ai, params, sent := photoBot(t, false)
	deliver(b, parse(t, dmPhoto))
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

// dmQuotedPhoto is a DM replying to an earlier photo, in the shape
// signal-cli 0.14 sends it (ids made up). The thumbnail travels with the new
// message, so it is fetched with the sender as recipient.
const dmQuotedPhoto = `{"envelope":{"source":"` + alice + `","sourceNumber":null,"sourceUuid":"` + alice + `","sourceName":"Alice","sourceDevice":2,"timestamp":2,` +
	`"dataMessage":{"timestamp":2,"message":"quoted image","expiresInSeconds":0,"isExpirationUpdate":false,"viewOnce":false,` +
	`"quote":{"id":1,"author":"` + alice + `","authorNumber":null,"authorUuid":"` + alice + `","text":"","attachments":[` +
	`{"contentType":"image/png","filename":null,"thumbnail":{"contentType":"image/png","filename":null,"id":"thumb2.png","size":6471,"width":null,"height":null,"caption":null,"isVoiceNote":false}}]}}},"account":"` + self + `"}`

func TestDMQuotedPhoto(t *testing.T) {
	b, ai, params, sent := photoBot(t, true)
	deliver(b, parse(t, dmQuotedPhoto))
	waitSent(t, sent)
	if p := <-params; p["id"] != "thumb2.png" || p["recipient"] != alice {
		t.Fatalf("getAttachment params %v", p)
	}
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if last := ai.msgs[len(ai.msgs)-1]; last.Content != "quoted image\n[1 image attached]" || len(last.Images) != 1 {
		t.Fatalf("model got %q with %d images", last.Content, len(last.Images))
	}
}

// groupPhoto is "@bot this is for you" with a photo, in a group, in
// signal-cli 0.14's shape (ids made up).
const groupPhoto = `{"envelope":{"source":"` + alice + `","sourceNumber":null,"sourceUuid":"` + alice + `","sourceName":"Alice","sourceDevice":1,"timestamp":4,` +
	`"dataMessage":{"timestamp":4,"message":"￼ this is for you","expiresInSeconds":0,"isExpirationUpdate":false,"viewOnce":false,` +
	`"mentions":[{"name":"` + self + `","number":null,"uuid":"` + self + `","start":0,"length":1}],` +
	`"attachments":[{"contentType":"image/png","filename":null,"id":"photo3.png","size":49372,"width":534,"height":534,"caption":null,"isVoiceNote":false}],` +
	`"groupInfo":{"groupId":"` + group + `","groupName":"g","revision":12,"type":"DELIVER"}}},"account":"` + self + `"}`

// A photo captioned with an @mention in a group is fetched by group id and
// shown to the model with the caption.
func TestGroupPhotoWithMention(t *testing.T) {
	b, ai, params, sent := photoBot(t, true)
	deliver(b, parse(t, groupPhoto))
	waitSent(t, sent)
	if p := <-params; p["id"] != "photo3.png" || p["groupId"] != group || p["recipient"] != nil {
		t.Fatalf("getAttachment params %v", p)
	}
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if last := ai.msgs[len(ai.msgs)-1]; last.Content != "Alice: this is for you\n[1 image attached]" || len(last.Images) != 1 {
		t.Fatalf("model got %q with %d images", last.Content, len(last.Images))
	}
}
