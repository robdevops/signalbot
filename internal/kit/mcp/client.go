package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/robdevops/signalbot/internal/kit/httpx"
)

const (
	// Protocol is the MCP protocol version this client speaks.
	Protocol = "2025-06-18"
	// CallTimeout bounds one request to a server.
	CallTimeout = 30 * time.Second
)

// Server is how to reach one MCP server: a URL for a remote one, or a
// command for a local one.
type Server struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// Tool is a tool a server offers.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

// Content is one item of a tool's result: text, or an image as base64.
type Content struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

// Result is what a tool call returned.
type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError"`
}

// Client is a connection to one server.
type Client struct {
	conn  conn
	stdio *stdioConn // nil for a remote server
}

// Dial connects to the server named name and does the MCP handshake,
// sending version as the client's version. A local server is started.
func Dial(ctx context.Context, name string, s Server, version string) (*Client, error) {
	c := &Client{}
	if s.URL != "" {
		c.conn = &httpConn{name: name, url: s.URL, headers: s.Headers, client: httpx.NewClient(), version: version}
	} else {
		c.stdio = &stdioConn{name: name, srv: s, version: version}
		if err := c.stdio.start(ctx); err != nil {
			return nil, err
		}
		c.conn = c.stdio
	}
	ictx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if err := initialize(ictx, c.conn, version); err != nil {
		return nil, err
	}
	return c, nil
}

// Local reports whether the server runs as a local command.
func (c *Client) Local() bool { return c.stdio != nil }

// Supervise restarts a local server whenever it exits, until ctx ends.
func (c *Client) Supervise(ctx context.Context) {
	if c.stdio != nil {
		c.stdio.supervise(ctx)
	}
}

// Tools lists every tool the server offers, following its pages.
func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	var tools []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.conn.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("tools/list: %v", err)
		}
		for _, t := range page.Tools {
			tools = append(tools, Tool{Name: t.Name, Description: t.Description, Schema: t.InputSchema})
		}
		if cursor = page.NextCursor; cursor == "" {
			return tools, nil
		}
	}
}

// ErrMalformed is a result that isn't valid JSON.
var ErrMalformed = fmt.Errorf("malformed result")

// Call runs a tool. An error from the server comes back as an *RPCError.
func (c *Client) Call(ctx context.Context, tool string, args map[string]any) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	raw, err := c.conn.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	if err != nil {
		return Result{}, err
	}
	var r Result
	if json.Unmarshal(raw, &r) != nil {
		return Result{}, ErrMalformed
	}
	return r, nil
}
