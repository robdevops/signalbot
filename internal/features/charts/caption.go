package charts

import (
	"github.com/robdevops/signalbot/internal/chat"
	"strings"
)

// chartNote opens a chart tool's result for the model.
const chartNote = "Chart attached. It is sent captioned with its title in place of your text, which won't be shown, so reply with just \"Done\". "

// captionMax is about the characters that fit on one line under a chart in
// Signal on a phone.
const captionMax = 36

// chartCaption is the text sent with a chart: "title — subtitle" if that fits
// on one line, else the title.
func chartCaption(title, subtitle string) string {
	if c := title + " — " + subtitle; subtitle != "" && len([]rune(c)) <= captionMax {
		return c
	}
	return title
}

// captions is the caption for a reply's charts, one per line, or "" if it
// has none.
func captions(pics []chat.Picture) string {
	var caps []string
	for _, p := range pics {
		if p.Caption != "" {
			caps = append(caps, p.Caption)
		}
	}
	return strings.Join(caps, "\n")
}
