package signal

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

func envelope(t *testing.T, s string) *Envelope {
	t.Helper()
	var e Envelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

const (
	inGroup  = `"groupInfo":{"groupId":"` + groupID + `"}`
	fromBob  = `"sourceUuid":"` + bobID + `","sourceNumber":"+1555","sourceName":"Bob"`
	mentions = `"mentions":[{"uuid":"` + botID + `","start":0,"length":1}]`
)

func TestAddressed(t *testing.T) {
	allowed := map[string]bool{"+1555": true}
	for _, tc := range []struct {
		name, env string
		want      bool
	}{
		{"mention", `{` + fromBob + `,"dataMessage":{"message":"￼ hi",` + mentions + `,` + inGroup + `}}`, true},
		{"mention by number", `{` + fromBob + `,"dataMessage":{"message":"￼ hi","mentions":[{"number":"+15550000","start":0,"length":1}],` + inGroup + `}}`, false},
		{"reply to the bot", `{` + fromBob + `,"dataMessage":{"message":"and?","quote":{"authorUuid":"` + botID + `"},` + inGroup + `}}`, true},
		{"reply to someone else", `{` + fromBob + `,"dataMessage":{"message":"and?","quote":{"authorUuid":"` + aliceID + `"},` + inGroup + `}}`, false},
		{"plain group message", `{` + fromBob + `,"dataMessage":{"message":"hi",` + inGroup + `}}`, false},
		{"DM from an allowed number", `{` + fromBob + `,"dataMessage":{"message":"hi"}}`, true},
		{"DM from anyone else", `{"sourceUuid":"` + aliceID + `","dataMessage":{"message":"hi"}}`, false},
		{"photo only DM", `{` + fromBob + `,"dataMessage":{"attachments":[{"contentType":"image/png","id":"a","size":10}]}}`, true},
		{"sticker only DM", `{` + fromBob + `,"dataMessage":{"sticker":{"packId":"p","stickerId":1}}}`, true},
		{"PDF only DM", `{` + fromBob + `,"dataMessage":{"attachments":[{"contentType":"application/pdf","id":"a","size":10}]}}`, false},
		{"empty DM", `{` + fromBob + `,"dataMessage":{"message":"  "}}`, false},
		{"no data message", `{` + fromBob + `}`, false},
		{"the bot's own", `{"sourceUuid":"` + botID + `","dataMessage":{"message":"hi"}}`, false},
	} {
		self := botID
		if got := Addressed(envelope(t, tc.env), self, allowed); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
		if Addressed(envelope(t, tc.env), "", allowed) {
			t.Errorf("%s: addressed before the bot's ID is known", tc.name)
		}
	}
}

func TestMessageInGroup(t *testing.T) {
	captureLogs(t)
	c := New("x")
	k := contact{UUID: aliceID}
	k.Profile.GivenName = "Alice"
	c.names.replace([]contact{k})
	// "@bot ask @Alice and @someone 🎉", replying to the bot with a photo.
	e := envelope(t, `{`+fromBob+`,"dataMessage":{"message":"￼ ask ￼ and ￼ 🎉","expiresInSeconds":3600,`+
		`"mentions":[{"uuid":"`+botID+`","start":0,"length":1},{"uuid":"unknown","name":"unknown","start":12,"length":1},{"uuid":"`+aliceID+`","name":"`+aliceID+`","start":6,"length":1}],`+
		`"attachments":[{"contentType":"image/jpeg","id":"a1","size":10},{"contentType":"image/png","id":"big","size":`+"99999999"+`,"thumbnail":{"contentType":"image/png","id":"t1","size":5}},{"contentType":"text/plain","id":"x","size":1}],`+
		`"quote":{"authorUuid":"`+botID+`","text":"earlier","attachments":[{"contentType":"image/png","id":"q1","size":5},{"contentType":"image/png","id":"q2","size":5},{"contentType":"image/png","id":"q3","size":5}]},`+
		`"sticker":{"packId":"p","stickerId":2},`+inGroup+`}}`)
	m := c.Message(e, botID, nil)
	want := feature.Message{
		Chat: feature.Chat{GroupID: groupID}, Sender: bobID, Name: "Bob",
		Text:    "ask @Alice and [unnamed user] 🎉",
		Quote:   &feature.Quote{Text: "earlier", Bot: true},
		Expires: time.Hour, Addressed: true,
		Media: []feature.Media{{ID: "a1", Type: "image/jpeg"}, {ID: "t1", Type: "image/png"},
			{ID: "q1", Type: "image/png", Quoted: true}, {ID: "q2", Type: "image/png", Quoted: true}, {Pack: "p", Sticker: 2}},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got  %+v\nwant %+v", m, want)
	}
	people := c.People(e, botID)
	if !reflect.DeepEqual(people, []Person{{"Bob", bobID}, {"Alice", aliceID}}) {
		t.Fatalf("people %+v", people)
	}
}

func TestMessageInDM(t *testing.T) {
	c := New("x")
	e := envelope(t, `{"sourceUuid":"`+aliceID+`","sourceName":"Alice\nSmith","dataMessage":{"message":"hi","quote":{"authorUuid":"`+bobID+`","text":"x"}}}`)
	m := c.Message(e, botID, map[string]bool{aliceID: true})
	if m.Chat != (feature.Chat{User: aliceID}) || m.Name != "Alice Smith" || !m.Addressed || m.Quote.Author != "" || m.Quote.Bot {
		t.Fatalf("got %+v %+v", m, m.Quote)
	}
	if p := c.People(e, botID); p != nil {
		t.Fatalf("a DM showed people: %v", p)
	}
	c.names.replace([]contact{{UUID: bobID, GivenName: "Bob"}})
	if m := c.Message(e, botID, nil); m.Quote.Author != "Bob" || m.Addressed {
		t.Fatalf("got %+v %+v", m, m.Quote)
	}
}

// Mentions out of order, overlapping or out of range never splice over the
// text.
func TestMentionsOddOffsets(t *testing.T) {
	captureLogs(t)
	c := New("x")
	for _, tc := range []struct {
		ms   []Mention
		want string
	}{
		{[]Mention{{UUID: botID, Start: 0, Length: 1}, {UUID: "u", Name: "Bob", Start: 4, Length: 1}}, "a @Bob"},
		{[]Mention{{UUID: "u", Name: "Bob", Start: 4, Length: 1}, {UUID: botID, Start: 0, Length: 1}}, "a @Bob"},
		{[]Mention{{UUID: "u", Name: "Bob", Start: 0, Length: 4}, {UUID: botID, Start: 2, Length: 2}}, "￼ ￼"},
		{[]Mention{{UUID: "u", Start: 99, Length: 1}, {UUID: "u", Start: 1, Length: -1}}, "￼ a ￼"},
	} {
		if got := c.text("￼ a ￼", tc.ms, botID, true); got != tc.want {
			t.Errorf("%v: %q", tc.ms, got)
		}
	}
}

// An unresolved mention is logged for an addressed message only: an
// unaddressed one is never logged.
func TestUnresolvedMentionLoggedOnlyWhenAddressed(t *testing.T) {
	logs := captureLogs(t)
	c := New("x")
	unknown := `"mentions":[{"uuid":"` + aliceID + `","start":0,"length":1}]`
	if m := c.Message(envelope(t, `{`+fromBob+`,"dataMessage":{"message":"￼ hi",`+unknown+`,`+inGroup+`}}`), botID, nil); m.Text != "[unnamed user] hi" {
		t.Fatalf("text %q", m.Text)
	}
	if logs.String() != "" {
		t.Fatalf("an unaddressed message logged %q", logs.String())
	}
	both := `"mentions":[{"uuid":"` + botID + `","start":0,"length":1},{"uuid":"` + aliceID + `","start":2,"length":1}]`
	c.Message(envelope(t, `{`+fromBob+`,"dataMessage":{"message":"￼ ￼",`+both+`,`+inGroup+`}}`), botID, nil)
	if !strings.Contains(logs.String(), "unresolved mention of "+aliceID) {
		t.Fatalf("logs %q", logs.String())
	}
}

func TestMentions(t *testing.T) {
	known := map[string]string{"alice": "u-a", "alice smith": "u-as", "bob": "u-b", "bob_2": "u-b2", "ambiguous": ""}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"no mentions here", nil},
		{"Hi @Alice!", []string{"3:6:u-a"}},
		{"hi @alice", []string{"3:6:u-a"}},
		{"hi @Alice Smith, ok", []string{"3:12:u-as"}},
		{"hi @Alicia", nil},
		{"mail me@Alice", nil},
		{"\U0001F389 @Alice!", []string{"3:6:u-a"}},
		{"@Bob and @bob_2", []string{"0:4:u-b", "9:6:u-b2"}},
		{"hi @Ambiguous and @Bob", []string{"18:4:u-b"}},
	} {
		if got := Mentions(tc.text, known); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
	if got := Mentions("@Alice", nil); got != nil {
		t.Errorf("no names: %v", got)
	}
}

func TestStripAt(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		found      bool
	}{
		{"@Bot hi", "hi", true},
		{"hi @bot, ok", "hi , ok", true},
		{"hi @signalbot", "hi", true},
		{"ask @Bot and @Bot again", "ask and again", true},
		{"hi @Botty", "hi @Botty", false},
		{"mail me@Bot", "mail me@Bot", false},
	} {
		got, found := StripAt(tc.text, "Bot", "signalbot")
		if got != tc.want || found != tc.found {
			t.Errorf("%q: %q %v", tc.text, got, found)
		}
	}
}

func TestParts(t *testing.T) {
	if got := Parts("@Bot hi @Alice", "Bot"); !reflect.DeepEqual(got, []string{"hi @Alice"}) {
		t.Errorf("got %q", got)
	}
	if got := Parts("  ", "Bot"); got != nil {
		t.Errorf("blank: %q", got)
	}
	long := strings.Repeat("word ", 300) + "\n" + strings.Repeat("x", 1500)
	got := Parts(long, "Bot")
	if len(got) != 2 || got[0] != strings.TrimSpace(strings.Repeat("word ", 300)) || got[1] != strings.Repeat("x", 1500) {
		t.Errorf("split at the line break: %d parts", len(got))
	}
	got = Parts(strings.Repeat("y", 3*MaxChars), "Bot")
	if len(got) != 3 || len([]rune(got[0])) != MaxChars {
		t.Errorf("hard split: %d parts", len(got))
	}
	got = Parts(strings.Repeat("z ", 3*MaxChars), "Bot")
	total := 0
	for _, p := range got {
		total += len([]rune(p))
	}
	if len(got) != 4 || total > MaxReply || !strings.HasSuffix(got[3], "[…truncated]") {
		t.Errorf("capped: %d parts, %d chars, ends %q", len(got), total, got[len(got)-1][max(0, len(got[len(got)-1])-20):])
	}
}
