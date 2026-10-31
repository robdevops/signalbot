package signal

import (
	"context"

	"encoding/json"
	"github.com/robdevops/signalbot/internal/chat"
	"testing"
)

// Placeholder ids: never real accounts.
const (
	self  = "c7e2a9d4-1b3f-4a86-8e05-6f9d2b4a7c31"
	alice = "3b1f6c2e-7a4d-4e58-9c13-0d2a5b8e71f4"
	group = "dGVzdC1ncm91cC1pZC1maXh0dXJlLTAwMDAwMDAwMDA="
)

func parse(t *testing.T, s string) *Envelope {
	t.Helper()
	var m struct{ Envelope *Envelope }
	if err := json.Unmarshal([]byte(s), &m); err != nil || m.Envelope == nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m.Envelope
}

const (
	typing       = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","timestamp":1,"typingMessage":{"action":"STARTED","timestamp":1,"groupId":"` + group + `"}},"account":"` + self + `"}`
	groupPlain   = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"Group test","groupInfo":{"groupId":"` + group + `","type":"DELIVER"}}},"account":"` + self + `"}`
	groupMention = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"￼ what is up","mentions":[{"name":"` + self + `","number":null,"uuid":"` + self + `","start":0,"length":1}],"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`
	groupReply   = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"Are you still there","quote":{"id":1,"author":"` + self + `","authorNumber":null,"authorUuid":"` + self + `","text":"oil around 78"},"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`
	dm           = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"Test DM"}},"account":"` + self + `"}`
)

// testAdapter is an adapter whose account id is known, with DMs allowed
// from allowed.
func testAdapter(allowed map[string]bool) *Adapter {
	a := NewAdapter("x", allowed)
	a.c.SetSelf(self)
	return a
}

// decide is what the adapter makes of env: the message and whether it is
// addressed, or ok false if the core would get nothing.
func decide(t *testing.T, a *Adapter, env *Envelope) (m chat.Message, addressed, ok bool) {
	t.Helper()
	var in *chat.Incoming
	a.inbound(env, func(i chat.Incoming) { in = &i })
	if in == nil {
		return chat.Message{}, false, false
	}
	m, ok = in.Load(context.Background())
	return m, in.Addressed, ok
}

// receiveLine is a "receive" notification carrying envelope.
func receiveLine(envelope string) string {
	return `{"jsonrpc":"2.0","method":"receive","params":` + envelope + "}\n"
}
