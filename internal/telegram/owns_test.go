package telegram

import (
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
)

// Telegram owns chats named by its numbers, and none of Signal's.
func TestOwns(t *testing.T) {
	var c Client
	for chat, want := range map[feature.Chat]bool{
		{User: "123456789"}:                                  true,
		{GroupID: "-1001234567890"}:                          true,
		{GroupID: "-1001234567890/42"}:                       true,
		{User: "11111111-1111-4111-8111-111111111111"}:       false,
		{User: "+61400000000"}:                               false,
		{GroupID: "Z3JvdXBJRGlzYmFzZTY0Zm9yU2lnbmFsMTIzNA="}: false,
		{}: false,
	} {
		if got := c.Owns(chat); got != want {
			t.Errorf("%+v: %v", chat, got)
		}
	}
}
