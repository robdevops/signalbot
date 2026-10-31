package signal

import (
	"encoding/json"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Send sends msg, with mentions ("start:length:uuid") and attachments (data:
// URIs, see imageDataURI).
func (s *Client) Send(t Target, msg string, mentions, attachments []string) error {
	p := t.params()
	p["message"] = msg
	if len(mentions) > 0 {
		p["mention"] = mentions
	}
	if len(attachments) > 0 {
		p["attachments"] = attachments
	}
	return s.write(s.nextID.Add(1), "send", p)
}

func (s *Client) Typing(t Target) {
	_ = s.write(s.nextID.Add(1), "sendTyping", t.params())
}

// handleUnmatched reports the outcome of fire-and-forget requests (send,
// sendTyping).
func (s *Client) handleUnmatched(m *rpcMsg) {
	if m.Error != nil {
		logx.Errorf("signal-cli request %s failed: %s", m.ID, m.Error.Message)
		return
	}
	var r struct {
		Results []struct {
			Type string `json:"type"`
		} `json:"results"`
	}
	if json.Unmarshal(m.Result, &r) != nil {
		return
	}
	for _, res := range r.Results {
		if res.Type != "" && res.Type != "SUCCESS" {
			logx.Warnf("signal-cli request %s not delivered: %s", m.ID, res.Type)
		}
	}
}
