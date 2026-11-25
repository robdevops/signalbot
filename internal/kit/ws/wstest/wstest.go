// Package wstest is the server side of a WebSocket, for tests of what dials
// one with package ws: a handler accepts the upgrade, then reads the
// client's messages and writes its own, frame by frame when a test needs
// fragments, pings or a close.
package wstest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Opcodes, as RFC 6455 numbers them.
const (
	Continuation = 0x0
	Text         = 0x1
	Binary       = 0x2
	Close        = 0x8
	Ping         = 0x9
	Pong         = 0xA
)

// Conn is the server's end of a WebSocket.
type Conn struct {
	c   net.Conn
	r   *bufio.Reader
	wmu sync.Mutex
}

// Accept answers w's upgrade request r and returns the connection.
func Accept(w http.ResponseWriter, r *http.Request) (*Conn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || r.Header.Get("Sec-WebSocket-Key") == "" {
		http.Error(w, "not a websocket", http.StatusBadRequest)
		return nil, errors.New("wstest: not a websocket request")
	}
	h := sha1.Sum([]byte(r.Header.Get("Sec-WebSocket-Key") + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	c, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, err
	}
	rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " +
		base64.StdEncoding.EncodeToString(h[:]) + "\r\n\r\n")
	if err := rw.Flush(); err != nil {
		c.Close()
		return nil, err
	}
	return &Conn{c: c, r: rw.Reader}, nil
}

// Frame is one frame as it came.
type Frame struct {
	Fin     bool
	Op      int
	Masked  bool
	Payload []byte // unmasked
}

// ReadFrame reads the client's next frame.
func (c *Conn) ReadFrame() (Frame, error) {
	var h [2]byte
	if _, err := io.ReadFull(c.r, h[:]); err != nil {
		return Frame{}, err
	}
	f := Frame{Fin: h[0]&0x80 != 0, Op: int(h[0] & 0x0F), Masked: h[1]&0x80 != 0}
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return Frame{}, err
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err := io.ReadFull(c.r, b[:]); err != nil {
			return Frame{}, err
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	var key [4]byte
	if f.Masked {
		if _, err := io.ReadFull(c.r, key[:]); err != nil {
			return Frame{}, err
		}
	}
	f.Payload = make([]byte, n)
	if _, err := io.ReadFull(c.r, f.Payload); err != nil {
		return Frame{}, err
	}
	if f.Masked {
		for i := range f.Payload {
			f.Payload[i] ^= key[i%4]
		}
	}
	return f, nil
}

// Read returns the client's next message, text or binary, answering its
// pings and skipping pongs; a close frame is returned as an error, after the
// close is answered.
func (c *Conn) Read() (string, error) {
	var msg []byte
	for {
		f, err := c.ReadFrame()
		if err != nil {
			return "", err
		}
		switch f.Op {
		case Ping:
			c.WriteFrame(true, Pong, f.Payload)
			continue
		case Pong:
			continue
		case Close:
			c.WriteFrame(true, Close, f.Payload)
			return "", errors.New("wstest: closed by the client")
		}
		msg = append(msg, f.Payload...)
		if f.Fin {
			return string(msg), nil
		}
	}
}

// Write sends s as one text message.
func (c *Conn) Write(s string) error { return c.WriteFrame(true, Text, []byte(s)) }

// WriteFrame sends one unmasked frame, as a server's are.
func (c *Conn) WriteFrame(fin bool, op int, payload []byte) error {
	b := []byte{byte(op)}
	if fin {
		b[0] |= 0x80
	}
	switch n := len(payload); {
	case n < 126:
		b = append(b, byte(n))
	case n <= 0xFFFF:
		b = append(b, 126)
		b = binary.BigEndian.AppendUint16(b, uint16(n))
	default:
		b = append(b, 127)
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := c.c.Write(append(b, payload...))
	return err
}

// CloseWith sends a close frame with code and reason, then closes.
func (c *Conn) CloseWith(code int, reason string) {
	c.WriteFrame(true, Close, append(binary.BigEndian.AppendUint16(nil, uint16(code)), reason...))
	c.c.Close()
}

// Drop closes the connection without a close frame, as a network failure
// does.
func (c *Conn) Drop() { c.c.Close() }
