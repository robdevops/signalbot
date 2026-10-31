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

// Tools lists every tool the server offers by name, following its pages.
func (c *Client) Tools(ctx context.Context) (map[string]mcpTool, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	tools := map[string]mcpTool{}
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
			tools[t.Name] = mcpTool{Name: t.Name, Description: t.Description, Schema: t.InputSchema}
		}
		if cursor = page.NextCursor; cursor == "" {
			return tools, nil
		}
	}
}
