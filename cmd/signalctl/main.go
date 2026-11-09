// Command signalctl sends one request to signal-cli's daemon, over the
// socket the bot uses, for one-off chores while the daemon owns the
// account: blocking someone, listing who is blocked, or any other
// JSON-RPC method. It is not part of the bot.
//
//	signalctl block +15551234567 group:Z3JvdXA=
//	signalctl unblock 3f9a…
//	signalctl blocked
//	signalctl call listGroups '{"detailed":true}'
//
// SIGNALCLI_SOCKET says where the daemon listens, as for the bot (default
// 127.0.0.1:7583). The daemon sends every connection the messages it
// receives; signalctl skips them unread, so it never prints anyone's text.
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

const usage = `usage:
  signalctl block <recipient>...     block people (number or ACI) or groups (group:<ID>)
  signalctl unblock <recipient>...   unblock them
  signalctl blocked                  list blocked people and groups
  signalctl call <method> [params]   send any JSON-RPC request; params is a JSON object
SIGNALCLI_SOCKET is the daemon's host:port or socket path (default 127.0.0.1:7583).`

// timeout bounds the whole request, from dialling to the reply.
var timeout = 15 * time.Second

func main() { os.Exit(run(os.Args[1:], os.Getenv("SIGNALCLI_SOCKET"), os.Stdout, os.Stderr)) }

func run(args []string, addr string, out, errOut io.Writer) int {
	addr = cmp.Or(strings.TrimSpace(addr), "127.0.0.1:7583")
	if len(args) == 0 {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	var err error
	switch cmd, rest := args[0], args[1:]; {
	case (cmd == "block" || cmd == "unblock") && len(rest) > 0:
		err = block(addr, cmd, rest, out)
	case cmd == "blocked" && len(rest) == 0:
		err = blocked(addr, out)
	case cmd == "call" && (len(rest) == 1 || len(rest) == 2):
		err = callRaw(addr, rest, out)
	default:
		fmt.Fprintln(errOut, usage)
		return 2
	}
	if err != nil {
		fmt.Fprintln(errOut, "signalctl:", err)
		return 1
	}
	return 0
}

// block blocks or unblocks recipients: people by number or ACI, groups as
// group:<ID>.
func block(addr, method string, recipients []string, out io.Writer) error {
	params := map[string][]string{}
	for _, r := range recipients {
		if id, ok := strings.CutPrefix(r, "group:"); ok {
			params["groupId"] = append(params["groupId"], id)
		} else {
			params["recipient"] = append(params["recipient"], r)
		}
	}
	if _, err := call(addr, method, params); err != nil {
		return err
	}
	fmt.Fprintf(out, "%sed %s\n", method, strings.Join(recipients, ", "))
	return nil
}

// blocked lists blocked people, then blocked groups, one per line.
func blocked(addr string, out io.Writer) error {
	raw, err := call(addr, "listContacts", map[string]any{"blocked": true})
	if err != nil {
		return err
	}
	var people []struct {
		Number, UUID, Name, GivenName string
		Profile                       struct{ GivenName string }
	}
	if err := json.Unmarshal(raw, &people); err != nil {
		return fmt.Errorf("listContacts: %w", err)
	}
	for _, p := range people {
		fmt.Fprintf(out, "person\t%s\t%s\n", cmp.Or(p.UUID, p.Number), cmp.Or(p.Name, p.GivenName, p.Profile.GivenName, "(no name)"))
	}
	if raw, err = call(addr, "listGroups", nil); err != nil {
		return err
	}
	var groups []struct {
		ID, Name  string
		IsBlocked bool
	}
	if err := json.Unmarshal(raw, &groups); err != nil {
		return fmt.Errorf("listGroups: %w", err)
	}
	for _, g := range groups {
		if g.IsBlocked {
			fmt.Fprintf(out, "group\tgroup:%s\t%s\n", g.ID, cmp.Or(g.Name, "(no name)"))
		}
	}
	return nil
}

// callRaw sends any method, with params as given, and prints the result
// indented.
func callRaw(addr string, args []string, out io.Writer) error {
	var params any
	if len(args) == 2 {
		if !json.Valid([]byte(args[1])) {
			return errors.New("params must be JSON, such as '{\"detailed\":true}'")
		}
		params = json.RawMessage(args[1])
	}
	raw, err := call(addr, args[0], params)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	if json.Indent(&b, raw, "", "  ") != nil {
		b.Write(raw)
	}
	fmt.Fprintln(out, b.String())
	return nil
}

// call sends one request on a fresh connection and returns its result,
// skipping the notifications the daemon sends every connection.
func call(addr, method string, params any) (json.RawMessage, error) {
	network := "unix" // the rule internal/signal's Client.network uses
	if _, _, err := net.SplitHostPort(addr); err == nil && !strings.ContainsRune(addr, '/') && !strings.HasPrefix(addr, "@") {
		network = "tcp"
	}
	conn, err := net.DialTimeout(network, addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("can't reach signal-cli at %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(timeout))
	req := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64<<10), 32<<20) // a listing, or a notification carrying a big attachment's metadata
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage
			Result json.RawMessage
			Error  *struct{ Message string }
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil || string(m.ID) != "1" {
			continue // a notification, such as a received message: never read further, never printed
		}
		if m.Error != nil {
			return nil, fmt.Errorf("%s: %s", method, m.Error.Message)
		}
		return m.Result, nil
	}
	if err := sc.Err(); err != nil {
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			return nil, fmt.Errorf("%s: no answer from signal-cli in %s", method, timeout)
		}
		return nil, err
	}
	return nil, fmt.Errorf("%s: signal-cli closed the connection without answering", method)
}
