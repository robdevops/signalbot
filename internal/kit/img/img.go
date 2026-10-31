// Package img checks images and encodes them for signal-cli.
package img

import (
	"encoding/base64"
	"net/http"
	"strings"
)

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

// Attachment is data as a signal-cli attachment named name, with the
// extension its type gives.
func Attachment(data []byte, name string) string {
	mime := http.DetectContentType(data)
	ext := strings.TrimPrefix(mime, "image/")
	return "data:" + mime + ";filename=" + name + "." + ext + ";base64," + base64.StdEncoding.EncodeToString(data)
}
