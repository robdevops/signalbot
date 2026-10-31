package bot

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

var png = []byte("\x89PNG\r\n\x1a\n0000")

// Each hook runs at its stage, in feature order, and what it returns counts
// only where the rules say.
func TestHooks(t *testing.T) {
	var turns []feature.Turn
	h := start(t, options{features: func(b *Bot) {
		b.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			switch {
			case !m.Addressed:
				return feature.Action{Context: m.Name + " said: " + m.Text, Answer: "never sent"}
			case m.Text == "drop it":
				return feature.Action{Ignore: true}
			}
			return feature.Action{Context: "never kept"}
		})
		b.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			if m.Text == "hello" {
				return feature.Action{Answer: "second hook"}
			}
			return feature.Action{}
		})
		b.OnTurn(func(_ context.Context, t feature.Turn) feature.Additions {
			turns = append(turns, t)
			return feature.Additions{System: "Be brief.", Note: "[a note]", HistoryNote: "[kept note]", Images: []string{"data:image/png;base64,AA=="}}
		})
		b.OnTurn(func(context.Context, feature.Turn) feature.Additions { return feature.Additions{Note: "[second note]"} })
		b.OnReply(func(_ feature.Turn, r *feature.Reply) {
			r.Text = ""
			r.Pictures = append(r.Pictures, feature.Picture{Data: png, Caption: "a caption", HistoryNote: "[sent a picture]"}, feature.Picture{Data: png})
		})
	}})
	h.receive(chatter("the moon is cheese"))
	h.receive(dm("drop it"))
	// The first hook's Context doesn't count for this one, so the second
	// answers it.
	h.receive(dm("hello"))
	if s := h.nextSend(); s["message"] != "second hook" {
		t.Fatalf("sent %v", s)
	}
	h.noSend()
	if len(h.requests()) != 0 {
		t.Fatal("the model was asked")
	}

	// When no hook acts, the model is asked.
	r, s := h.ask(atBot("look"))
	if !strings.Contains(system(r), "\nBe brief.\nIf asked to introduce yourself") {
		t.Fatalf("system %q", system(r))
	}
	equal(t, "model got", contents(r), []string{"user: Bob said: the moon is cheese", "user: Alice: look\n[a note]\n[second note]"})
	if parts := messages(r)[2]["content"].([]any); len(parts) != 2 {
		t.Fatalf("content %v", parts)
	}
	if s["message"] != "a caption" || len(s["attachments"].([]any)) != 2 || !strings.HasPrefix(s["attachments"].([]any)[0].(string), "data:image/png;filename=image1.png;base64,") {
		t.Fatalf("sent %v", s)
	}
	if len(turns) != 1 || turns[0].Text != "look" || turns[0].Chat.GroupID != groupID {
		t.Fatalf("turns %+v", turns)
	}
	r, _ = h.ask(atBot("again"))
	equal(t, "history", contents(r)[1:3], []string{"user: Alice: look\n[kept note]\n[second note]", "assistant: [sent a picture]\n[sent an image]\n"})
}

// A photo alone is answered only when a hook adds something to it.
func TestPhotoAnsweredWhenAHookAdds(t *testing.T) {
	var media []byte
	var h *harness
	h = start(t, options{features: func(b *Bot) {
		b.OnTurn(func(ctx context.Context, t feature.Turn) feature.Additions {
			if len(t.Media) == 0 {
				return feature.Additions{}
			}
			media, _ = b.Media(ctx, t.Message, 0)
			return feature.Additions{Note: "[1 image attached]"}
		})
	}})
	h.set("getAttachment", `{"data":"`+base64.StdEncoding.EncodeToString(png)+`"}`)
	r, _ := h.ask(msg{from: aliceID, name: "Alice", images: []string{"photo1"}})
	equal(t, "model got", contents(r), []string{"user: [1 image attached]"})
	if string(media) != string(png) || h.calledWith("getAttachment")[0]["id"] != "photo1" {
		t.Fatalf("media %q, calls %v", media, h.calledWith("getAttachment"))
	}
}

// Commands answer without the model or choose a tool; a tool choice offers
// only that tool.
func TestCommands(t *testing.T) {
	h := start(t, options{tools: true, features: func(b *Bot) {
		feature.Command(b, "hi", func(_ context.Context, m feature.Message, args string) feature.Action {
			return feature.Action{Answer: "hello " + args}
		})
		feature.Command(b, "pick", func(context.Context, feature.Message, string) feature.Action {
			return feature.Action{ToolChoice: "b_tool"}
		})
		b.Tools(group("alpha", "a_tool", nil))
		b.Tools(group("beta", "b_tool", regexp.MustCompile(`\bplot\b`)))
	}})
	h.receive(dm("/HI there"))
	if s := h.nextSend(); s["message"] != "hello there" || len(h.requests()) != 0 {
		t.Fatalf("sent %v", s)
	}
	r, _ := h.ask(dm("/pick"))
	equal(t, "tools", toolNames(r), []string{"b_tool"})
	if c, _ := json.Marshal(r["tool_choice"]); string(c) != `{"function":{"name":"b_tool"},"type":"function"}` {
		t.Fatalf("tool_choice %s", c)
	}
	if !strings.Contains(system(r), "\nYour tools: alpha; beta. If asked what tools you have: alpha, beta.\n") {
		t.Fatalf("system %q", system(r))
	}
	h.receive(chatter("/hi")) // not addressed: not a command
	h.noSend()
}

// A group with Keywords is offered when a message matches, and for the 2
// replies after one that matched or used its tools.
func TestOffering(t *testing.T) {
	h := start(t, options{tools: true, features: func(b *Bot) {
		b.Tools(group("alpha", "a_tool", nil))
		b.Tools(group("beta", "b_tool", regexp.MustCompile(`\bplot\b`)))
	}})
	for _, c := range []struct {
		text    string
		tools   []string
		listing string
	}{
		{"hello", []string{"a_tool"}, "Your tools: alpha. "},
		{"plot it", []string{"a_tool", "b_tool"}, "Your tools: alpha; beta. "},
		{"more", []string{"a_tool", "b_tool"}, "Your tools: alpha; beta. "},
		{"more", []string{"a_tool", "b_tool"}, "Your tools: alpha; beta. "},
		{"enough", []string{"a_tool"}, "Your tools: alpha. "},
	} {
		r, _ := h.ask(dm(fmt.Sprint(c.text, " ", len(h.requests())))) // not a repeat
		equal(t, c.text, toolNames(r), c.tools)
		want := "\n" + c.listing + "If asked what tools you have: alpha, beta.\n"
		if s := system(r); !strings.Contains(s, want) || !strings.Contains(s, "(tools: alpha, beta)") || strings.Contains(s, "beta system") != slices.Contains(c.tools, "b_tool") {
			t.Fatalf("%s: system %q", c.text, s)
		}
	}
	// A reply that used the group's tool starts its follow-ups again: two
	// more after it, not one.
	h.ask(dm("plot that"))
	h.answer(toolCall("b_tool"), text("done"))
	h.receive(dm("more of that"))
	h.nextSend()
	for i, want := range [][]string{{"a_tool", "b_tool"}, {"a_tool", "b_tool"}, {"a_tool"}} {
		r, _ := h.ask(dm(fmt.Sprint("next ", i)))
		equal(t, "follow-up", toolNames(r), want)
	}
	if info := h.b.Info(feature.Chat{}); info.Name != "Botty" || info.Model != "test/test-model" || !info.CallsTools || !info.Images || strings.Join(info.Tools, ",") != "alpha,beta" {
		t.Fatalf("info %+v", info)
	}
}

// A model that can't call tools is offered none, and {tools} is "none".
func TestNoToolsWithoutToolCalls(t *testing.T) {
	h := start(t, options{features: func(b *Bot) { b.Tools(group("alpha", "a_tool", nil)) }})
	r, _ := h.ask(dm("hello"))
	if _, ok := r["tools"]; ok || strings.Contains(system(r), "If asked what tools you have") || !strings.Contains(system(r), "(tools: none)") {
		t.Fatalf("tools %v, system %q", r["tools"], system(r))
	}
	if !strings.Contains(h.logs.String(), "<4>model test/test-model can't call tools, so tools are off\n") {
		t.Fatalf("logs:\n%s", h.logs)
	}
}

func group(name, tool string, keywords *regexp.Regexp) feature.ToolGroup {
	return feature.ToolGroup{Names: []string{name}, Listing: name, System: name + " system", Keywords: keywords,
		Tools: []feature.Tool{{Name: tool, Description: "a tool", Run: func(context.Context, string) (string, []feature.Picture, error) {
			return "result", nil, nil
		}}}}
}

func toolCall(name string) func() (int, string) {
	return func() (int, string) {
		return 200, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"` + name + `","arguments":"{}"}}]}}]}`
	}
}

// A delivered message is posted as its sender's, answered if it mentions
// the bot, and dropped if its sender left the group. A timer delivers.
func TestDeliver(t *testing.T) {
	h := start(t, options{})
	group := feature.Chat{GroupID: groupID}
	h.b.At("later", time.Now(), func(_ context.Context, due time.Time) {
		h.b.Deliver(feature.Message{Chat: group, Sender: aliceID, Name: "Alice", Text: "see you @bob and @Alice"})
	})
	s := h.nextSend()
	if s["message"] != "see you @bob and @Alice" || s["groupId"] != groupID {
		t.Fatalf("sent %v", s)
	}
	if m, _ := s["mention"].([]any); len(m) != 2 || m[0] != "8:4:"+bobID || m[1] != "17:6:"+aliceID {
		t.Fatalf("mentions %v", s["mention"])
	}
	h.b.Deliver(feature.Message{Chat: group, Sender: aliceID, Text: "@BOTTY what's new?"}) // her name is looked up
	r := h.nextSend()
	equal(t, "model got", contents(h.requests()[0]), []string{"user: Alice: see you @bob and @Alice", "user: Alice: what's new?"})
	if r["message"] != "ok" || !strings.Contains(h.logs.String(), "<6>reply Alice ") {
		t.Fatalf("sent %v; logs:\n%s", r, h.logs)
	}
	h.b.Deliver(feature.Message{Chat: feature.Chat{GroupID: "other"}, Sender: aliceID, Text: "hello"})
	h.eventually("the drop logged", func() bool {
		return strings.Contains(h.logs.String(), "<6>delivered message dropped: its sender is not in the group\n")
	})
	h.noSend()
	gs, err := h.b.Groups(context.Background(), bobID)
	if err != nil || len(gs) != 1 || gs[0] != (feature.Group{ID: groupID, Name: "Family"}) {
		t.Fatalf("groups %v %v", gs, err)
	}
	if gs, _ := h.b.Groups(context.Background(), "+1999"); len(gs) != 0 {
		t.Fatalf("groups %v", gs)
	}
}
