// Package httpx makes the bot's outbound HTTP clients.
package httpx

import (
	"crypto/tls"
	"net/http"
)

// NewClient is Go's default transport (proxy settings, timeouts, HTTP/2),
// but refusing anything older than TLS 1.3.
func NewClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	return &http.Client{Transport: t}
}
