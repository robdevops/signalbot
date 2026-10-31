package httpx

import (
	"crypto/tls"
	"net/http"
	"testing"
)

func TestClient(t *testing.T) {
	tr := Client(32).Transport.(*http.Transport)
	if tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Error("TLS 1.3 not required")
	}
	if tr.MaxIdleConnsPerHost != 32 || tr.MaxIdleConns < 32 {
		t.Errorf("idle conns %d per host, %d in all", tr.MaxIdleConnsPerHost, tr.MaxIdleConns)
	}
	if tr.Proxy == nil {
		t.Error("proxy settings dropped")
	}
}
