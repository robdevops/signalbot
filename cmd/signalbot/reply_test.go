package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"github.com/robdevops/signalbot/internal/llm"
	"image"
	"image/png"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/transports/signal"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
)

// tinyPNG is a 1×1 PNG, base64-encoded.
func tinyPNG() string {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// imageAI replies with text and one image, captioned if it is a chart.
type imageAI struct{ text, caption string }

// A reply is one message: a chart with its caption in place of the model's
// text, or an MCP image with the text, or the image alone.
func TestReplyCarriesImages(t *testing.T) {
	for _, tc := range []struct{ text, caption, want string }{
		{"Done", "NVDA vs AMD", "NVDA vs AMD"},
		{"look", "", "look"},
		{"", "", ""},
	} {
		text := tc.text
		sent := make(chan map[string]any, 2)
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
				case "send":
					sent <- req["params"].(map[string]any)
				}
			}
		})
		sig := signal.NewAdapter(sock, nil)
		b := NewBot(context.Background(), testCfg(), imageAI{text, tc.caption}, sig)
		ctx, cancel := context.WithCancel(context.Background())
		go b.Run(ctx)
		sig.Client().WaitSelf(5 * time.Second)
		deliver(b, parse(t, groupMention))
		next := func() map[string]any {
			select {
			case p := <-sent:
				return p
			case <-time.After(5 * time.Second):
				t.Fatalf("text %q: no reply", text)
				return nil
			}
		}
		p := next()
		att, _ := p["attachments"].([]any)
		if p["message"] != tc.want || len(att) != 1 || !strings.HasPrefix(att[0].(string), "data:image/png;filename=image1.png;base64,") {
			t.Errorf("text %q: message %q, attachments %v", text, p["message"], att)
		}
		select {
		case p := <-sent:
			t.Errorf("text %q: a second message %v", text, p)
		case <-time.After(200 * time.Millisecond):
		}
		cancel()
	}
}

func (a imageAI) Complete(context.Context, llm.Request) (llm.Completion, error) {
	img, _ := base64.StdEncoding.DecodeString(tinyPNG())
	return llm.Completion{Text: a.text, Images: []llm.Image{{Data: img, Caption: a.caption}}}, nil
}
