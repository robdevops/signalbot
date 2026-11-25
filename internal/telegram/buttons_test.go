package telegram

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
)

// Getting updates asks for button presses too.
func TestPollsPresses(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.poll(context.Background(), 0)
	kinds, _ := f.callsTo("getUpdates")[0].Params["allowed_updates"].([]any)
	if !slices.Contains(kinds, any("callback_query")) {
		t.Fatalf("allowed_updates %v", kinds)
	}
}

// A press is the core's, with the message's ID and the button's data, and
// its Answer shows a toast.
func TestPressed(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	press := func(from, chat string) (bot.Received, bool) {
		return c.event(parse(t, `{"update_id":1,"callback_query":{"id":"q1",`+from+`,"message":{"message_id":500,`+chat+`,"date":1},"data":"rain 7d"}}`))
	}
	r, ok := press(fromA, inDM)
	p, isPress := r.(bot.Pressed)
	if !ok || !isPress || p.Press.Chat != (feature.Chat{User: "1111111"}) || p.Press.Sent != 500 || p.Press.Data != "rain 7d" || p.Press.By.ID != "1111111" || p.Press.By.Name != "Alice Ames" {
		t.Fatalf("press %+v", r)
	}
	p.Answer("7 days " + strings.Repeat("x", 300))
	waitCalls(t, f, "answerCallbackQuery", 1)
	a := f.callsTo("answerCallbackQuery")[0].Params
	if a["callback_query_id"] != "q1" || !strings.HasPrefix(a["text"].(string), "7 days x") || len([]rune(a["text"].(string))) > maxToast {
		t.Fatalf("answer %v", a)
	}
	if r, ok := press(fromA, inGroup); !ok || r.(bot.Pressed).Press.Chat.GroupID != groupS {
		t.Fatalf("in a group %+v", r)
	}
	// With TELEGRAM_DM_USER_IDS "all", anyone's press is the core's, Bob's in
	// his own DM too.
	if r, ok := press(`"from":{"id":2222222,"first_name":"Bob"}`, `"chat":{"id":2222222,"type":"private"}`); !ok || r.(bot.Pressed).Press.Chat.User != "2222222" {
		t.Fatalf("a stranger's press %+v", r)
	}
}

// Buttons go on the post's last message as an inline keyboard; a DM's
// keyboard as a persistent reply keyboard, or taken away; silent posts go
// without a notification. A group gets no reply keyboard.
func TestSendMarkup(t *testing.T) {
	f := newFake(t)
	f.results["sendPhoto"] = `{"message_id":601}`
	f.results["sendVoice"] = `{"message_id":701}`
	c := connected(f.client())
	buttons := feature.Markup{Silent: true, Buttons: [][]feature.Button{{{Label: "24h", Data: "rain 24h"}, {Label: "7d", Data: "rain 7d"}}}}
	inline := `{"inline_keyboard":[[{"callback_data":"rain 24h","text":"24h"},{"callback_data":"rain 7d","text":"7d"}]]}`
	asJSON := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return string(b)
	}

	// Text past a message's limit: the buttons on the last part.
	c.Send(feature.Chat{GroupID: groupS}, strings.Repeat("word ", 1000), nil, nil, 0, nil, buttons)
	msgs := f.callsTo("sendMessage")
	if len(msgs) != 2 || msgs[0].Params["reply_markup"] != nil || asJSON(msgs[1].Params["reply_markup"]) != inline {
		t.Fatalf("parts %d, markup %v", len(msgs), msgs)
	}
	for _, m := range msgs {
		if m.Params["disable_notification"] != true {
			t.Fatalf("not silent: %v", m.Params)
		}
	}

	// A photo with its caption: the buttons on the photo.
	png := []byte("\x89PNG\r\n\x1a\n")
	c.Send(feature.Chat{User: "1111111"}, "Rain", []feature.Picture{{Data: png}}, nil, 0, nil, buttons)
	ph := f.callsTo("sendPhoto")
	if len(ph) != 1 || ph[0].Params["reply_markup"] != inline || ph[0].Params["disable_notification"] != "true" || len(f.callsTo("sendMessage")) != 2 {
		t.Fatalf("photo %+v", ph)
	}

	// An album, silent too.
	f.results["sendMediaGroup"] = `[{"message_id":602},{"message_id":603}]`
	c.Send(feature.Chat{User: "1111111"}, "Rain", []feature.Picture{{Data: png}, {Data: png}}, nil, 0, nil, feature.Markup{Silent: true})
	if mg := f.callsTo("sendMediaGroup"); len(mg) != 1 || mg[0].Params["disable_notification"] != "true" {
		t.Fatalf("album %+v", mg)
	}

	// A clip alone: the buttons on it.
	c.Send(feature.Chat{User: "1111111"}, "", nil, nil, 0, []byte("ID3"), buttons)
	if v := f.callsTo("sendVoice"); len(v) != 1 || v[0].Params["reply_markup"] != inline || v[0].Params["disable_notification"] != "true" {
		t.Fatalf("voice %+v", v)
	}

	// A DM's keyboard, then none; a group gets neither.
	keys := feature.Markup{Keyboard: [][]feature.Key{{{Label: "🌧 Rain", Command: "/rain"}}}}
	c.Send(feature.Chat{User: "1111111"}, "Pick", nil, nil, 0, nil, keys)
	c.Send(feature.Chat{User: "1111111"}, "Bye", nil, nil, 0, nil, feature.Markup{NoKeyboard: true})
	c.Send(feature.Chat{GroupID: groupS}, "Pick", nil, nil, 0, nil, keys)
	msgs = f.callsTo("sendMessage")[2:]
	if got := asJSON(msgs[0].Params["reply_markup"]); got != `{"is_persistent":true,"keyboard":[[{"text":"🌧 Rain"}]],"resize_keyboard":true}` {
		t.Errorf("keyboard %s", got)
	}
	if got := asJSON(msgs[1].Params["reply_markup"]); got != `{"remove_keyboard":true}` {
		t.Errorf("no keyboard %s", got)
	}
	if msgs[2].Params["reply_markup"] != nil || msgs[0].Params["disable_notification"] != nil {
		t.Errorf("in a group %v", msgs[2].Params)
	}
}

// An edit replaces a message's text, a photo's caption when it has no
// text, or its photo, with the buttons given, and with none only its
// buttons; "not modified" is done, and a refusal fails it.
func TestEdit(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	dm := feature.Chat{User: "1111111"}
	buttons := feature.Markup{Buttons: [][]feature.Button{{{Label: "7d", Data: "rain 7d"}}}}
	inline := map[string]any{"inline_keyboard": []any{[]any{map[string]any{"text": "7d", "callback_data": "rain 7d"}}}}

	if got := c.Edit(dm, 500, "**Rain**, 7 days", nil, nil, buttons); !reflect.DeepEqual(got, []int64{500}) {
		t.Fatalf("edit %v", got)
	}
	e := f.callsTo("editMessageText")
	if len(e) != 1 || e[0].Params["text"] != "Rain, 7 days" || e[0].Params["message_id"] != float64(500) || e[0].Params["chat_id"] != "1111111" ||
		!reflect.DeepEqual(e[0].Params["reply_markup"], inline) || e[0].Params["entities"] == nil {
		t.Fatalf("editMessageText %+v", e)
	}

	// A photo's message has no text: its caption, without buttons now.
	f.errs["editMessageText"] = `{"ok":false,"error_code":400,"description":"Bad Request: there is no text in the message to edit"}`
	if got := c.Edit(dm, 501, "Rain", nil, nil, feature.Markup{}); !reflect.DeepEqual(got, []int64{501}) {
		t.Fatalf("caption edit %v", got)
	}
	if e := f.callsTo("editMessageCaption"); len(e) != 1 || e[0].Params["caption"] != "Rain" || e[0].Params["reply_markup"] != nil || e[0].Params["text"] != nil {
		t.Fatalf("editMessageCaption %+v", e)
	}

	// A new photo.
	png := []byte("\x89PNG\r\n\x1a\n")
	c.Edit(dm, 502, "Rain", []feature.Picture{{Data: png}}, nil, buttons)
	m := f.callsTo("editMessageMedia")
	if len(m) != 1 || string(m[0].Files["p0"]) != string(png) || m[0].Params["message_id"] != "502" ||
		!strings.Contains(m[0].Params["media"].(string), `"caption":"Rain"`) || !strings.Contains(m[0].Params["reply_markup"].(string), "rain 7d") {
		t.Fatalf("editMessageMedia %+v", m)
	}

	// No text, no photo: only the buttons.
	c.Edit(dm, 503, "", nil, nil, buttons)
	if r := f.callsTo("editMessageReplyMarkup"); len(r) != 1 || !reflect.DeepEqual(r[0].Params["reply_markup"], inline) {
		t.Fatalf("editMessageReplyMarkup %+v", r)
	}

	// Not modified is done; another refusal fails.
	f.errs["editMessageText"] = `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`
	if got := c.Edit(dm, 500, "same", nil, nil, feature.Markup{}); !reflect.DeepEqual(got, []int64{500}) {
		t.Fatalf("not modified %v", got)
	}
	f.errs["editMessageText"] = `{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`
	if got := c.Edit(dm, 499, "gone", nil, nil, feature.Markup{}); got != nil {
		t.Fatalf("a refused edit %v", got)
	}

	// A photo for a message Telegram won't put one on: a new message, with
	// the buttons.
	f.errs["editMessageMedia"] = `{"ok":false,"error_code":400,"description":"Bad Request: there is no media in the message to edit"}`
	f.results["sendPhoto"] = `{"message_id":601}`
	if got := c.Edit(dm, 504, "Rain", []feature.Picture{{Data: png}}, nil, buttons); !reflect.DeepEqual(got, []int64{601}) {
		t.Fatalf("a photo sent anew %v", got)
	}
	if ph := f.callsTo("sendPhoto"); len(ph) != 1 || !strings.Contains(ph[0].Params["reply_markup"].(string), "rain 7d") || ph[0].Params["disable_notification"] != nil {
		t.Fatalf("sendPhoto %+v", ph)
	}
}

// A press's answer past Telegram's limit, in UTF-16 units, is cut to fit;
// one Telegram refuses is answered again without text, so the button stops
// spinning.
func TestAnswerPress(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	c.answerPress("q1", strings.Repeat("🌧", 150))
	got := f.callsTo("answerCallbackQuery")[0].Params["text"].(string)
	if n := len(utf16.Encode([]rune(got))); n > maxToast || !strings.HasSuffix(got, "…") {
		t.Fatalf("%d units: %q", n, got)
	}
	if s := strings.Repeat("🌧", 100); toast(s) != s {
		t.Fatal("a toast that fits, cut")
	}
	f.errs["answerCallbackQuery"] = `{"ok":false,"error_code":400,"description":"Bad Request: MESSAGE_TOO_LONG"}`
	c.answerPress("q2", "Rain")
	a := f.callsTo("answerCallbackQuery")
	if len(a) != 3 || a[1].Params["text"] != "Rain" || a[2].Params["text"] != nil {
		t.Fatalf("answers %+v", a)
	}
}

// A highlighted button is sent in Telegram's "primary" style, blue.
func TestHighlightedButton(t *testing.T) {
	kb := inlineKeyboard([][]feature.Button{{{Label: "On", Data: "a", Highlight: true}, {Label: "Off", Data: "b"}}})
	rows := kb["inline_keyboard"].([][]map[string]string)
	if rows[0][0]["style"] != "primary" || rows[0][1]["style"] != "" {
		t.Fatalf("%+v", rows)
	}
}
