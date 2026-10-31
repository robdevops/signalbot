package charts

import (
	"regexp"
	"slices"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/bound"
)

// followUps is how many replies after one that asked for or drew a chart
// still offer the chart tools, for follow-ups like "show that for a month".
const followUps = 2

// maxChats bounds the chats counted: chat ids come from the network.
const maxChats = 500

// wantsChart reports whether a message asks for a chart, so the chart tools
// are offered only then.
var wantsChart = regexp.MustCompile(`(?i)\b(chart(s|ed|ing)?|graph(s|ed|ing)?|plot(s|ted|ting)?|visuali[sz](e|es|ed|ing|ation|ations)|pies?|donuts?|doughnuts?)\b`).MatchString

// offer is on when the message asks for a chart, or a recent one did; never
// on a chat command's turn, which offers only its own tool.
func (f *Feature) offer(t feature.Turn) bool {
	return t.Forced == "" && (wantsChart(t.Message.Text) || f.turns(t.Message.Chat) > 0)
}

func (f *Feature) turns(c chat.ChatID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.charts[c]
}

// replied counts a reply that offered the chart tools: one that asked for
// or drew a chart restarts the count, any other uses one up. A reply with
// charts is sent as their captions.
func (f *Feature) replied(t feature.Turn, r feature.Reply) *chat.Outgoing {
	if slices.Contains(t.Offered, chartTool) {
		drew := slices.ContainsFunc(r.Used, func(n string) bool { return n == chartTool || n == donutTool })
		f.count(t.Message.Chat, wantsChart(t.Message.Text) || drew)
	}
	if c := captions(r.Pictures); c != "" {
		return &chat.Outgoing{Chat: t.Message.Chat, Text: c}
	}
	return nil
}

func (f *Feature) count(c chat.ChatID, fresh bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case fresh:
		bound.MakeRoom(f.charts, c, maxChats)
		f.charts[c] = followUps
	case f.charts[c] > 1:
		f.charts[c]--
	default:
		delete(f.charts, c)
	}
}

// forget drops a chat's count with its history: nothing left to follow up on.
func (f *Feature) forget(c chat.ChatID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.charts, c)
}
