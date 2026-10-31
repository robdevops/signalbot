package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// conn is a connection to one MCP server.
type conn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
}

// RPCError is a JSON-RPC error from an MCP server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// initialize performs the MCP handshake.
func initialize(ctx context.Context, c conn, version string) error {
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": Protocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "signalbot", "version": version},
	}); err != nil {
		return err
	}
	return c.notify(ctx, "notifications/initialized", nil)
}
