// Package httpx makes the HTTP client for outside services.
package httpx

import (
	"crypto/tls"
	"net/http"
)

// Client is Go's default transport (proxy settings, timeouts, HTTP/2), but
// refusing anything older than TLS 1.3 and keeping up to idle connections to
// each host, where Go's default keeps 2.
func Client(idle int) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	t.MaxIdleConnsPerHost = max(idle, 2)
	t.MaxIdleConns = max(t.MaxIdleConns, t.MaxIdleConnsPerHost)
	return &http.Client{Transport: t}
}
