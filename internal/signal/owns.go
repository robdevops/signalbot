package signal

import (
	"regexp"
	"strings"

	"github.com/robdevops/signalbot/internal/feature"
)

// A Signal user is a UUID or a phone number starting "+", never digits
// alone; a group's ID is base64, ending "=", never a number. Those are
// Telegram's (internal/telegram/owns.go). Neither has a ":", which other
// networks' chats start with ("discord:…", "slack:…").
var (
	digits  = regexp.MustCompile(`^[0-9]+$`)
	numeric = regexp.MustCompile(`^-?[0-9]+(/[0-9]+)?$`)
)

// Owns reports whether chat is a Signal chat, by the shape of its ID, so a
// bot also on another network sends it here.
func (c *Client) Owns(chat feature.Chat) bool {
	if chat.GroupID != "" {
		return !numeric.MatchString(chat.GroupID) && !strings.Contains(chat.GroupID, ":")
	}
	return chat.User != "" && !digits.MatchString(chat.User) && !strings.Contains(chat.User, ":")
}
