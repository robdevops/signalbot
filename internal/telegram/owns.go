package telegram

import (
	"regexp"

	"github.com/robdevops/signalbot/internal/feature"
)

// Telegram's chats are named by numbers: a user by a positive one, a group
// by a negative one, and a forum topic by its group's, "/" and the topic's.
var (
	userID  = regexp.MustCompile(`^[0-9]+$`)
	groupID = regexp.MustCompile(`^-?[0-9]+(/[0-9]+)?$`)
)

// Owns reports whether chat is a Telegram chat, by the shape of its ID, so
// a bot also on another network sends it here.
func (c *Client) Owns(chat feature.Chat) bool {
	if chat.GroupID != "" {
		return groupID.MatchString(chat.GroupID)
	}
	return userID.MatchString(chat.User)
}
