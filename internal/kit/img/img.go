// Package img checks images the bot sends and receives.
package img

import (
	"net/http"
)

// Sendable reports whether data is an image Signal can show, small
// enough to send.
func Sendable(data []byte) bool {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return len(data) <= MaxBytes
	}
	return false
}

// MaxBytes caps one image, sent or received.
const MaxBytes = 5 << 20

// MaxPerMessage caps the images taken from one message, or sent with one
// reply.
const MaxPerMessage = 4
