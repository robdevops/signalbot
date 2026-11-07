package bot

import (
	"context"

	"github.com/robdevops/signalbot/internal/feature"
)

// Received is what the chat network hands the core, or what waits in a
// chat's mailbox: one of the types below. Only this package can add one,
// and features never see them.
type Received interface{ chat() feature.Chat }

// Pending is a message completed in its slot: an addressed one, or one that
// arrived before the bot knew its own ID, which Load then decides. Load may
// call the network, and calls learn with each name it shows the model and
// that person's ID. It reports false for a message the core has nothing to
// do with.
type Pending struct {
	Chat feature.Chat
	Load func(ctx context.Context, learn func(name, id string)) (feature.Message, bool)
}

// Chatter is a group message not for the bot, for the OnMessage hooks.
type Chatter struct{ Message feature.Message }

// GroupChanged says only that a group's members, name or description changed.
type GroupChanged struct{ Chat feature.Chat }

// Happened is an event: a reaction or a change in a group's members.
type Happened struct{ Event feature.Event }

// Posted is a message a feature delivers as its sender (Deliver), waiting
// in its chat's mailbox for the network to ready it.
type Posted struct{ Message feature.Message }

// Told is a text the bot sends a user in their DM (Tell), waiting in that
// chat's mailbox.
type Told struct{ User, Text string }

func (r Pending) chat() feature.Chat      { return r.Chat }
func (r Told) chat() feature.Chat         { return feature.Chat{User: r.User} }
func (r Chatter) chat() feature.Chat      { return r.Message.Chat }
func (r GroupChanged) chat() feature.Chat { return r.Chat }
func (r Happened) chat() feature.Chat     { return r.Event.Chat }
func (r Posted) chat() feature.Chat       { return r.Message.Chat }

// Ready is a posted message as the network readies it: its sender's name
// filled in, the bot's @name stripped, and in a group its members.
type Ready struct {
	Message   feature.Message
	Mentioned bool     // the text had the bot's @name, now stripped
	Members   []Member // a group's members, whose @Name a post links
}
