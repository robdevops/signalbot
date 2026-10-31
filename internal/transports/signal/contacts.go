package signal

import (
	"context"
	"encoding/json"
)

// Contact is the subset of a signal-cli listContacts entry we use.
type Contact struct {
	Number    string `json:"number"`
	UUID      string `json:"uuid"`
	Username  string `json:"username"`
	GivenName string `json:"givenName"`
	Profile   struct {
		GivenName string `json:"givenName"`
	} `json:"profile"`
}

// ListContacts returns the account's contacts and other known recipients.
func (s *Client) ListContacts(ctx context.Context) ([]Contact, error) {
	resp, err := s.call(ctx, "listContacts", map[string]any{"allRecipients": true})
	if err != nil {
		return nil, err
	}
	var cs []Contact
	if err := json.Unmarshal(resp.Result, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}
