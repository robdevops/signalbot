// Package img checks images.
package img

import "net/http"

// MaxBytes caps one image, sent or received.
const MaxBytes = 5 << 20

// Supported reports whether mime is an image type Signal shows and models
// take.
func Supported(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Sendable reports whether data is an image Signal can show and models
// take, small enough to send.
func Sendable(data []byte) bool {
	return len(data) <= MaxBytes && Supported(http.DetectContentType(data))
}
