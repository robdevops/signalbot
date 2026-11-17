package signal

import (
	"fmt"

	"github.com/robdevops/signalbot/internal/kit/mdstyle"
)

// styleNames are signal-cli's names for the text styles.
var styleNames = map[mdstyle.Style]string{mdstyle.Bold: "BOLD", mdstyle.Italic: "ITALIC", mdstyle.Strike: "STRIKETHROUGH", mdstyle.Mono: "MONOSPACE", mdstyle.Spoiler: "SPOILER"}

// textStyles is the signal-cli text styles ("start:length:STYLE", in UTF-16
// units) of a message's runes, one for each run of a style.
func textStyles(s mdstyle.Text) []string {
	var out []string
	for _, r := range s.Runs() {
		out = append(out, fmt.Sprintf("%d:%d:%s", r.Start, r.Len, styleNames[r.Style]))
	}
	return out
}
