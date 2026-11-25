package signal

import (
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
)

// Signal owns chats named by UUIDs, phone numbers and base64 group IDs,
// and none named by Telegram's numbers.
func TestOwns(t *testing.T) {
	var c Client
	for chat, want := range map[feature.Chat]bool{
		{User: "11111111-1111-4111-8111-111111111111"}:       true,
		{User: "+61400000000"}:                               true,
		{GroupID: "Z3JvdXBJRGlzYmFzZTY0Zm9yU2lnbmFsMTIzNA="}: true,
		{User: "123456789"}:                                  false,
		{GroupID: "-1001234567890/42"}:                       false,
		{GroupID: "discord:123456789012345678"}:              false,
		{User: "slack:U012AB3CD"}:                            false,
		{}:                                                   false,
	} {
		if got := c.Owns(chat); got != want {
			t.Errorf("%+v: %v", chat, got)
		}
	}
}
