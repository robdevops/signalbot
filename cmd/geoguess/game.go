package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/cmd/geoguess/geo"
)

// The texts players see.
const (
	guard     = "No hints during a round 😉"
	noChat    = "GeoGuess works only in a chat."
	noRound   = "There's no round on. Start one with /geo."
	noPhoto   = "I'm still looking for a photo. Try again in a minute."
	badLength = "A round can be 30s to 30m, such as /geo 90s or /geo 5m."
	usage     = "Try /geo, /geo 90s, /geo end, /geo skip, /geo scores, or a region such as /geo europe or /geo AU,NZ."
)

// pin is the reaction to a recorded guess.
const pin = "📍"

// call runs one tool. Only an unknown tool is a protocol error; the rest
// answer in words.
func (s *server) call(ctx context.Context, p callParams) (any, error) {
	switch p.Name {
	case "geo":
		var a struct {
			Args string `json:"args"`
		}
		if err := params(p.Arguments, &a); err != nil {
			return nil, err
		}
		return s.command(ctx, p.Meta.Context, a.Args), nil
	case "geo_guess":
		var a struct {
			Text string `json:"text"`
		}
		if err := params(p.Arguments, &a); err != nil {
			return nil, err
		}
		return s.guess(p.Meta.Context, a.Text), nil
	}
	return nil, &rpcError{codeInvalidParams, "unknown tool"}
}

// text is a tool result of words alone.
func text(s string) toolResult { return toolResult{Content: []content{{Type: "text", Text: s}}} }

// fromChat reports whether c names a chat and a sender.
func fromChat(c *callContext) bool {
	return c != nil && validID(c.Chat, "c_") && validID(c.Sender, "u_")
}

// command is /geo with its arguments.
func (s *server) command(ctx context.Context, c *callContext, args string) toolResult {
	if !fromChat(c) {
		return toolResult{Content: text(noChat).Content, IsError: true}
	}
	fields := strings.Fields(args)
	if len(fields) == 1 {
		switch strings.ToLower(fields[0]) {
		case "end":
			return s.finish(c, false)
		case "skip":
			return s.finish(c, true)
		case "scores":
			return text(s.scoreboard(c.Chat))
		}
	}
	length := s.cfg.Round
	var regions []string
	for _, f := range fields {
		if f[0] >= '0' && f[0] <= '9' {
			d, err := roundLength(strings.ToLower(f))
			if err != nil {
				return text(badLength)
			}
			length = d
			continue
		}
		regions = append(regions, f)
	}
	region := strings.Join(regions, ",")
	if region != "" {
		if _, err := s.regionSeeds(region); err != nil {
			return text("I don't know that region. " + usage)
		}
	}
	return s.start(ctx, c, length, region)
}

// busy is the answer to /geo while a round is on.
func busy(r *round, now time.Time) toolResult {
	left := max(r.Deadline.Sub(now), 0).Round(time.Second)
	m, sec := int(left/time.Minute), int(left%time.Minute/time.Second)
	var label string
	switch {
	case m == 0:
		label = fmt.Sprintf("%ds", sec)
	case sec == 0:
		label = fmt.Sprintf("%dm", m)
	default:
		label = fmt.Sprintf("%dm%ds", m, sec)
	}
	return text("A round is on: " + label + " left.")
}

// start begins a round in c's chat with a photo from the buffer, and
// answers with the photo, its caption and the watch for guesses.
func (s *server) start(ctx context.Context, c *callContext, length time.Duration, region string) toolResult {
	s.mu.Lock()
	r := s.rounds[c.Chat]
	s.mu.Unlock()
	if r != nil {
		return busy(r, s.clock.Now())
	}
	photo, ok := s.take(ctx, region)
	if !ok {
		return text(noPhoto)
	}
	s.mu.Lock()
	now := s.clock.Now()
	if r := s.rounds[c.Chat]; r != nil { // another /geo began one meanwhile
		s.buffer = append([]geo.Photo{photo}, s.buffer...)
		s.mu.Unlock()
		return busy(r, now)
	}
	r = &round{
		ID: "r" + randomID(6), Chat: c.Chat, Starter: c.Sender,
		Started: now, Deadline: now.Add(length),
		Answer: photo.At, Place: photo.Place.Name,
		Credit:  credit{Creator: photo.Creator, Source: photo.Source, Licence: photo.Licence, Link: photo.Link},
		Guesses: map[string]guess{},
	}
	s.rounds[c.Chat] = r
	s.arm(r)
	network := s.network
	s.mu.Unlock()
	s.save()
	s.log.Printf("round %s started: %s, a %s photo", r.ID, length, photo.Source)
	return toolResult{
		Content: []content{
			{Type: "image", Data: base64.StdEncoding.EncodeToString(photo.JPEG), MimeType: "image/jpeg"},
			{Type: "text", Text: caption(r.Deadline, length, network)},
		},
		Meta: &resultMeta{Watch: &watch{
			ID: r.ID, Pattern: geo.Pattern, Until: r.Deadline.Add(watchSlack).Format(time.RFC3339),
			Tool: "geo_guess", Guard: guard,
		}},
	}
}

// caption is the photo's caption: how to guess, on the chat network the
// bot said it is on, and by when.
func caption(deadline time.Time, length time.Duration, network string) string {
	steps := "Signal: **+** → Location; Telegram: 📎 → Location. Pinch to zoom out, **drag the pin**, confirm (on Signal), then send."
	switch network {
	case "signal":
		steps = "Tap **+** → Location, pinch to zoom out, **drag the pin**, confirm, then send."
	case "telegram":
		steps = "Tap 📎 → Location, pinch to zoom out, **drag the pin**, then send."
	}
	return "🌍 **Where is this?** Send a location before " + deadline.Local().Format("15:04") + " (" + lengthLabel(length) + ").\n" +
		steps + " Or type coordinates, e.g. `-37.81, 144.96`.\n" +
		"⚠️ Location starts at *your* position. Move the pin before you send."
}

// lengthLabel is a round's length as the caption says it: 3 min, 90 sec.
func lengthLabel(d time.Duration) string {
	if d%time.Minute == 0 {
		return strconv.Itoa(int(d/time.Minute)) + " min"
	}
	return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + " sec"
}

// arm sets r's timer to reveal it once its time is up. s.mu is held.
func (s *server) arm(r *round) {
	chat, id := r.Chat, r.ID
	s.timers[chat] = s.clock.AfterFunc(r.Deadline.Add(settle).Sub(s.clock.Now()), func() { s.timeUp(chat, id) })
}

// timeUp reveals the round id in chat, if it is still on.
func (s *server) timeUp(chat, id string) {
	s.mu.Lock()
	r := s.rounds[chat]
	if r == nil || r.ID != id {
		s.mu.Unlock()
		return
	}
	s.timeUpLocked(r)
	s.mu.Unlock()
	s.poke()
	s.save()
}

// timeUpLocked ends r and queues its results and the end of its watch for
// the bot to post. s.mu is held.
func (s *server) timeUpLocked(r *round) {
	results := s.end(r, true)
	s.queue(op{Op: "post", Chat: r.Chat, Text: results})
	s.queue(op{Op: "unwatch", Chat: r.Chat, Watch: r.ID})
}

// finish is /geo end (skip false) or /geo skip, which only the player who
// started the round may use. It answers with the results, and queues the
// end of the round's watch.
func (s *server) finish(c *callContext, skip bool) toolResult {
	s.mu.Lock()
	r := s.rounds[c.Chat]
	switch {
	case r == nil:
		s.mu.Unlock()
		return text(noRound)
	case r.Starter != c.Sender && skip:
		s.mu.Unlock()
		return text("Only the player who started this round can skip it.")
	case r.Starter != c.Sender:
		s.mu.Unlock()
		return text("Only the player who started this round can end it.")
	}
	results := s.end(r, !skip)
	s.queue(op{Op: "unwatch", Chat: r.Chat, Watch: r.ID})
	s.mu.Unlock()
	s.poke()
	s.save()
	return text(results)
}

// end closes r, adding its points to the chat's scores if scored, and
// returns its results. s.mu is held.
func (s *server) end(r *round, scored bool) string {
	delete(s.rounds, r.Chat)
	if t := s.timers[r.Chat]; t != nil {
		t.Stop()
		delete(s.timers, r.Chat)
	}
	guesses := make([]geo.Guess, 0, len(r.Guesses))
	for user, g := range r.Guesses {
		guesses = append(guesses, geo.Guess{User: user, Km: g.Km, At: g.At})
	}
	placings := geo.Rank(guesses)
	how := "revealed"
	if scored {
		now := s.clock.Now()
		s.pruneScores(now)
		for _, p := range placings {
			s.scores[r.Chat] = append(s.scores[r.Chat], score{User: p.User, Points: p.Points, At: now})
		}
	} else {
		how = "skipped"
	}
	s.log.Printf("round %s %s: %d guesses", r.ID, how, len(placings))
	return results(r, placings, scored)
}

// results is a round's reveal: the place, then each player's distance and
// points, never where they guessed, then the photo's credit. Players are
// {u_…} placeholders, which the bot turns into names.
func results(r *round, placings []geo.Placing, scored bool) string {
	place := "📍 [**" + plain(r.Place) + "**](" + mapLink(r.Answer) + ")"
	var b strings.Builder
	switch {
	case !scored:
		b.WriteString("Round skipped. It was " + place)
	case len(placings) == 0:
		b.WriteString("Nobody guessed. It was " + place)
	default:
		b.WriteString(place)
		for _, p := range placings {
			b.WriteString("\n" + medal(p.Rank) + " {" + p.User + "} " + geo.Distance(p.Km) + " · " + geo.Thousands(p.Points))
		}
	}
	b.WriteString("\n" + r.Credit.String())
	return b.String()
}

// String is the credit line: "Photo: jsmith, Mapillary, CC BY-SA 4.0",
// linked to the photo's page.
func (c credit) String() string {
	var parts []string
	for _, p := range []string{plain(c.Creator), c.Source, c.Licence} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	line := strings.Join(parts, ", ")
	if strings.HasPrefix(c.Link, "https://") && !strings.ContainsAny(c.Link, " ()") {
		line = "[" + line + "](" + c.Link + ")"
	}
	return "Photo: " + line
}

// plain is s without the characters Markdown would read, so a photo's
// author can't style or link the results.
func plain(s string) string {
	return strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if strings.ContainsRune("[]()*_`~\\<>|#", r) {
			return -1
		}
		return r
	}, s)), " ")
}

// mapLink is a map of the answer on OpenStreetMap.
func mapLink(p geo.Point) string {
	lat, lon := strconv.FormatFloat(p.Lat, 'f', 5, 64), strconv.FormatFloat(p.Lon, 'f', 5, 64)
	return "https://www.openstreetmap.org/?mlat=" + lat + "&mlon=" + lon + "#map=12/" + lat + "/" + lon
}

// medal marks a rank: a medal for the first three, else the number.
func medal(rank int) string {
	if rank <= 3 {
		return []string{"🥇", "🥈", "🥉"}[rank-1]
	}
	return strconv.Itoa(rank) + "."
}

// guess records the location in a chat message as its sender's guess in
// the chat's round: only how far it is from the answer, never where it is.
// A recorded guess answers with a pin for the bot to react with.
func (s *server) guess(c *callContext, msg string) toolResult {
	if !fromChat(c) {
		return text(noChat)
	}
	p, ok := geo.Parse(msg)
	if !ok {
		return text("No location found.")
	}
	at := s.clock.Now()
	if t := cmp.Or(c.Edited, c.Sent); t > 0 {
		at = time.Unix(t, 0)
	}
	s.mu.Lock()
	r := s.rounds[c.Chat]
	switch {
	case r == nil:
		s.mu.Unlock()
		return text(noRound)
	case c.Sent > 0 && c.Sent < r.Started.Unix():
		s.mu.Unlock()
		return text("That message is from before this round.")
	case at.After(r.Deadline):
		s.mu.Unlock()
		return text("Too late for this round.")
	}
	r.Guesses[c.Sender] = guess{Km: geo.Km(p, r.Answer), At: at}
	s.mu.Unlock()
	s.save()
	return toolResult{Content: text("Recorded.").Content, Meta: &resultMeta{React: pin}}
}

// scoreboard is /geo scores: the chat's totals over the last scoreDays.
func (s *server) scoreboard(chat string) string {
	s.mu.Lock()
	s.pruneScores(s.clock.Now())
	type total struct {
		user           string
		points, rounds int
	}
	byUser := map[string]*total{}
	var totals []*total
	for _, sc := range s.scores[chat] {
		t := byUser[sc.User]
		if t == nil {
			t = &total{user: sc.User}
			byUser[sc.User] = t
			totals = append(totals, t)
		}
		t.points += sc.Points
		t.rounds++
	}
	s.mu.Unlock()
	if len(totals) == 0 {
		return "No scores in the last 7 days. Start a round with /geo."
	}
	// Scores are kept in the order they were won, so a tie goes to who
	// scored first.
	slices.SortStableFunc(totals, func(a, b *total) int { return cmp.Compare(b.points, a.points) })
	var b strings.Builder
	b.WriteString("🏆 **Scores, last 7 days**")
	for i, t := range totals[:min(len(totals), 10)] {
		rounds := "1 round"
		if t.rounds != 1 {
			rounds = strconv.Itoa(t.rounds) + " rounds"
		}
		fmt.Fprintf(&b, "\n%s {%s} %s · %s", medal(i+1), t.user, geo.Thousands(t.points), rounds)
	}
	return b.String()
}

// pruneScores forgets scores older than scoreDays. s.mu is held.
func (s *server) pruneScores(now time.Time) {
	since := now.Add(-scoreDays * 24 * time.Hour)
	for chat, list := range s.scores {
		s.scores[chat] = slices.DeleteFunc(list, func(sc score) bool { return sc.At.Before(since) })
		if len(s.scores[chat]) == 0 {
			delete(s.scores, chat)
		}
	}
}
