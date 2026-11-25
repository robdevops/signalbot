package mcp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"maps"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// maxArg caps the text a direct command passes as its argument, in runes.
const maxArg = 200

// plainDirect runs a plain server's direct commands: the bot calls a tool,
// or reads a resource, itself, and sends what comes back without the
// model. Unlike an addon's, the call carries no chat context: a plain
// server learns nothing of who asked or where.
type plainDirect struct {
	server, title string
	conn          conn
	busy          chan struct{} // a place for each call in flight
}

func newPlainDirect(server, title string, c conn) *plainDirect {
	return &plainDirect{server: server, title: title, conn: c, busy: make(chan struct{}, maxDirect)}
}

// run answers /name with text, the rest of the message: c's tool called
// with c's args, the text as its arg if it has one, or c's resource read.
func (d *plainDirect) run(ctx context.Context, name string, c chatCommand, text string) feature.Action {
	args, usage := c.arguments(text)
	if usage != "" {
		return feature.Action{Answer: strings.ReplaceAll(usage, "{name}", name)}
	}
	select {
	case d.busy <- struct{}{}:
		defer func() { <-d.busy }()
	default:
		logx.Warnf("MCP server %s: /%s refused: %d calls already in flight", d.server, name, maxDirect)
		return feature.Action{Answer: d.title + " is busy, try again in a moment."}
	}
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(c.timeout, directTimeout))
	defer cancel()
	start := time.Now()
	method, params, what := "tools/call", map[string]any{"name": c.Tool, "arguments": args}, c.Tool
	if c.Resource != "" {
		method, params, what = "resources/read", map[string]any{"uri": c.Resource}, "a resource"
	}
	res, err := d.conn.call(ctx, method, params)
	var r directResult
	outcome := "ok"
	switch {
	case err != nil:
		outcome = errCode(err)
	case json.Unmarshal(res, &r) != nil:
		outcome = "a malformed result"
	case r.IsError:
		outcome = "an error result"
	}
	logx.Infof("MCP server %s: /%s read %s: %s in %dms, %d bytes", d.server, name, what, outcome, time.Since(start).Milliseconds(), len(res))
	if outcome != "ok" {
		return feature.Action{Answer: d.title + " couldn't do that just now."}
	}
	act := r.action(d.server)
	if act.Answer == "" && len(act.Pictures) == 0 {
		act.Answer = d.title + " had nothing to show."
	}
	return act
}

// arguments are the tool's arguments for text: c's args, with text as
// c.Arg when it has one (one of c.Values, in any case, when it has those).
// usage, if not "", is the answer instead, with "{name}" for the command.
func (c chatCommand) arguments(text string) (args map[string]any, usage string) {
	args = maps.Clone(c.Args)
	if args == nil {
		args = map[string]any{}
	}
	text = strings.TrimSpace(text)
	if c.Arg == "" || text == "" {
		return args, "" // a command without an argument ignores any text after it
	}
	if len(c.Values) == 0 {
		args[c.Arg] = txt.Clamp(text, maxArg)
		return args, ""
	}
	for _, v := range c.Values {
		if strings.EqualFold(v, text) {
			args[c.Arg] = v
			return args, ""
		}
	}
	return nil, "Use /{name} alone, or with one of: " + strings.Join(c.Values, ", ") + "."
}

// directResult is a tools/call or resources/read result: a tool's content,
// or a resource's contents, each text or an image.
type directResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"content"`
	Contents []struct {
		Text     string `json:"text"`
		Blob     string `json:"blob"`
		MimeType string `json:"mimeType"`
	} `json:"contents"`
	IsError bool `json:"isError"`
}

// action is what the result says in chat: its text, and its images made
// safe, at most feature.MaxPictures.
func (r directResult) action(server string) feature.Action {
	var parts, images []string
	for _, p := range r.Content {
		switch p.Type {
		case "text":
			parts = append(parts, p.Text)
		case "image":
			images = append(images, p.Data)
		}
	}
	for _, p := range r.Contents {
		switch {
		case p.Blob != "" && strings.HasPrefix(p.MimeType, "image/"):
			images = append(images, p.Blob)
		case p.Text != "":
			parts = append(parts, p.Text)
		}
	}
	var pics []feature.Picture
	left := 0
	for _, b64 := range images {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err == nil {
			data, err = img.Safe(data)
		}
		if err != nil || !img.Sendable(data) || len(pics) == feature.MaxPictures {
			left++
			continue
		}
		pics = append(pics, feature.Picture{Data: data})
	}
	if left > 0 {
		logx.Warnf("MCP server %s: %d images left out: not images, too big, or past %d", server, left, feature.MaxPictures)
	}
	return feature.Action{Answer: txt.Clamp(strings.TrimSpace(strings.Join(parts, "\n")), maxAnswer), Pictures: pics}
}
