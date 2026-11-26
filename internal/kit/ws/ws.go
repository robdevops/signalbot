// Package ws is a WebSocket client (RFC 6455), as much of one as the chat
// networks that push over WebSockets need: text and binary messages, ping
// and close, no extensions. The handshake is an ordinary request through an
// http.Client, so it keeps the bot's proxy, TLS and timeout settings; Go
// sends a request asking to upgrade to a WebSocket over HTTP/1.1, which has
// the upgrade HTTP/2 lacks, and hands back the connection in the 101
// response's Body. This package only frames messages on it.
package ws

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Opcodes, RFC 6455 section 5.2.
const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// maxControl is the most a control frame (ping, pong, close) may carry.
const maxControl = 125

// ErrTooLarge is a message past the connection's read limit; the
// connection is closed with status 1009.
var ErrTooLarge = errors.New("ws: message too large")

// CloseError is the peer's close: its status code and reason.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: closed by peer: %d %s", e.Code, e.Reason)
}

// Conn is a client WebSocket. Read and Write may be called at the same
// time, from different goroutines; Writes, and the pongs Read sends, take
// turns.
type Conn struct {
	rwc   io.ReadWriteCloser
	r     *bufio.Reader
	limit int64

	wmu    sync.Mutex // one frame at a time
	closed bool       // a close frame sent; under wmu
	once   sync.Once
}

// Dial opens a WebSocket at url (ws:// or wss://, or http(s)://) with
// client, which must not follow redirects to another host blindly: Go's
// default does, which suits the APIs this is for. header is sent with the
// handshake (an Authorization, say). limit is the largest message Read
// accepts, in bytes.
func Dial(ctx context.Context, client *http.Client, url string, header http.Header, limit int64) (*Conn, error) {
	switch {
	case strings.HasPrefix(url, "wss://"):
		url = "https://" + url[len("wss://"):]
	case strings.HasPrefix(url, "ws://"):
		url = "http://" + url[len("ws://"):]
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	key := make([]byte, 16)
	rand.Read(key)
	k := base64.StdEncoding.EncodeToString(key)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", k)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		resp.Body.Close()
		return nil, fmt.Errorf("ws: handshake: HTTP %d", resp.StatusCode)
	}
	rwc, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		resp.Body.Close()
		return nil, errors.New("ws: handshake: the connection can't be written")
	}
	if !strings.EqualFold(resp.Header.Get("Upgrade"), "websocket") || resp.Header.Get("Sec-WebSocket-Accept") != accept(k) {
		rwc.Close()
		return nil, errors.New("ws: handshake: not a WebSocket answer")
	}
	return &Conn{rwc: rwc, r: bufio.NewReader(rwc), limit: limit}, nil
}

// accept is the Sec-WebSocket-Accept a server answers key with.
func accept(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Read returns the next message, text or binary, reassembled from its
// frames. It answers pings with pongs as they come, and returns a
// *CloseError when the peer closes, after answering it. When ctx ends, the
// connection is closed, so Read returns.
func (c *Conn) Read(ctx context.Context) (text bool, msg []byte, err error) {
	stop := context.AfterFunc(ctx, func() { c.rwc.Close() })
	defer stop()
	op := -1
	for {
		fin, frameOp, payload, err := c.frame()
		if err != nil {
			if ctx.Err() != nil {
				return false, nil, ctx.Err()
			}
			return false, nil, err
		}
		switch frameOp {
		case opPing:
			if err := c.write(opPong, payload); err != nil {
				return false, nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			code, reason := 1005, ""
			if len(payload) >= 2 {
				code, reason = int(binary.BigEndian.Uint16(payload)), string(payload[2:])
			}
			c.closeWith(1000, "")
			return false, nil, &CloseError{Code: code, Reason: reason}
		case opText, opBinary:
			if op != -1 {
				return false, nil, c.fail("a new message inside a fragmented one")
			}
			op, msg = frameOp, payload
		case opContinuation:
			if op == -1 {
				return false, nil, c.fail("a continuation with no message")
			}
			if int64(len(msg)+len(payload)) > c.limit {
				c.closeWith(1009, "")
				return false, nil, ErrTooLarge
			}
			msg = append(msg, payload...)
		default:
			return false, nil, c.fail(fmt.Sprintf("unknown opcode %d", frameOp))
		}
		if fin {
			return op == opText, msg, nil
		}
	}
}

// frame reads one frame: a server's is never masked.
func (c *Conn) frame() (fin bool, op int, payload []byte, err error) {
	var h [2]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return false, 0, nil, err
	}
	fin, op = h[0]&0x80 != 0, int(h[0]&0x0F)
	if h[0]&0x70 != 0 {
		return false, 0, nil, c.fail("reserved bits set")
	}
	if h[1]&0x80 != 0 {
		return false, 0, nil, c.fail("a masked frame from the server")
	}
	n := int64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return false, 0, nil, err
		}
		n = int64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return false, 0, nil, err
		}
		if u := binary.BigEndian.Uint64(b[:]); u > 1<<62 {
			n = 1 << 62
		} else {
			n = int64(u)
		}
	}
	if op >= opClose && (n > maxControl || !fin) {
		return false, 0, nil, c.fail("a control frame too long or fragmented")
	}
	if n > c.limit {
		c.closeWith(1009, "")
		return false, 0, nil, ErrTooLarge
	}
	payload = make([]byte, n)
	if _, err := io.ReadFull(c.r, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// fail closes the connection for a protocol error (1002).
func (c *Conn) fail(why string) error {
	c.closeWith(1002, "")
	return errors.New("ws: protocol error: " + why)
}

// Write sends msg as one text message, or binary if !text. When ctx ends,
// the connection is closed, so Write returns.
func (c *Conn) Write(ctx context.Context, text bool, msg []byte) error {
	stop := context.AfterFunc(ctx, func() { c.rwc.Close() })
	defer stop()
	op := opBinary
	if text {
		op = opText
	}
	if err := c.write(op, msg); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	return nil
}

// write sends one frame, masked as a client's must be.
func (c *Conn) write(op int, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("ws: closed")
	}
	if op == opClose {
		c.closed = true
	}
	return writeFrame(c.rwc, op, payload)
}

// writeFrame writes one final, masked frame.
func writeFrame(w io.Writer, op int, payload []byte) error {
	b := make([]byte, 0, 14+len(payload))
	b = append(b, 0x80|byte(op))
	switch n := len(payload); {
	case n < 126:
		b = append(b, 0x80|byte(n))
	case n <= 0xFFFF:
		b = append(b, 0x80|126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 0x80|127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	var key [4]byte
	rand.Read(key[:])
	b = append(b, key[:]...)
	start := len(b)
	b = append(b, payload...)
	for i := range payload {
		b[start+i] ^= key[i%4]
	}
	_, err := w.Write(b)
	return err
}

// Close closes the connection with status code (1000 for a normal close)
// and reason, telling the peer first, then closing at once without waiting
// for its answer: the clients here never reuse a connection.
func (c *Conn) Close(code int, reason string) error {
	c.closeWith(code, reason)
	return nil
}

// closeWith sends a close frame, if none was sent, and closes the
// connection, once.
func (c *Conn) closeWith(code int, reason string) {
	c.once.Do(func() {
		if len(reason) > maxControl-2 {
			reason = reason[:maxControl-2]
		}
		p := binary.BigEndian.AppendUint16(nil, uint16(code))
		if t, ok := c.rwc.(interface{ SetWriteDeadline(time.Time) error }); ok {
			t.SetWriteDeadline(time.Now().Add(time.Second))
		}
		c.write(opClose, append(p, reason...))
		c.rwc.Close()
	})
}
