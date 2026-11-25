// Package mcp offers the model the tools of the MCP servers in MCP_CONFIG,
// one tool group per server, and turns them into chat commands. A local
// server with an "addon" block that speaks signalbot's extension is an
// addon: the bot calls its tools itself, for direct commands, and it never
// learns who anyone is, only pseudonyms.
package mcp

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	protocol    = "2025-06-18"
	callTimeout = 30 * time.Second
	// maxResult caps what one tool call adds to the conversation, in runes.
	maxResult = 16 << 10
)

// server is one entry of MCP_CONFIG's "mcpServers": a URL for a remote
// server, or a command for a local one. allowed_tools, when present, offers
// the model only those tools; blocked_tools, when present, offers all but
// those. Without either, every tool the server lists is offered.
type server struct {
	URL          string                 `json:"url"`
	Headers      map[string]string      `json:"headers"`
	Command      string                 `json:"command"`
	Args         []string               `json:"args"`
	Env          map[string]string      `json:"env"`
	AllowedTools []string               `json:"allowed_tools"` // nil: not in whitelist mode
	BlockedTools []string               `json:"blocked_tools"`
	Disabled     bool                   `json:"disabled"` // left out entirely
	ChatCommands map[string]chatCommand `json:"chat_commands"`
	Instructions *string                `json:"instructions"` // a system-prompt line while its tools are offered; unset: the server's own, "": none
	Keywords     []string               `json:"keywords"`     // if set, its tools are offered only when a message has one of these words
	Addon        *addonConfig           `json:"addon"`        // set for an addon
}

// chatCommand makes "/name ..." a turn in which the model must call tool,
// or, if direct, an addon's tool the bot calls itself, without the model.
// With require_arg, a bare "/name" is answered by the bot, not the model,
// showing example if set. A direct command's timeout is how long its call
// may take, from 1s to 2m; unset, directTimeout.
type chatCommand struct {
	Tool       string `json:"tool"`
	RequireArg bool   `json:"require_arg"`
	Example    string `json:"example"`
	Direct     bool   `json:"direct"`
	Timeout    string `json:"timeout"`

	timeout time.Duration // Timeout, read; 0 if unset
}

// settings are the keys a server may have, and commandSettings a chat
// command's; others are a typo, refused at startup. ignored are keys other
// MCP clients write that the bot has no use for, so a pasted entry starts.
var (
	settings = []string{"url", "headers", "command", "args", "env", "allowed_tools", "blocked_tools",
		"instructions", "keywords", "chat_commands", "disabled", "addon"}
	commandSettings = []string{"tool", "require_arg", "example", "direct", "timeout"}
	ignored         = []string{"type", "transport", "timeout", "alwaysAllow", "autoApprove", "description"}
)

var unsafeToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// envPattern finds ${NAME} in a server's settings, for the bot's environment
// variable NAME.
var envPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// Register starts the servers in MCP_CONFIG, if set, and offers their tools
// on h. They run as long as the bot: when it stops, local servers have
// their stdin closed, then get SIGTERM, then SIGKILL, and the bot waits for
// them. With no model, only addons are started. With an addon, the
// pseudonyms' secret and each addon's watches, claims, leases and outbox
// cursor are kept in the "mcp" store; if it can't be opened, addons run as
// plain servers.
func Register(h feature.Host) error {
	path := h.Env("MCP_CONFIG", "")
	if path == "" {
		return nil
	}
	servers, err := load(path)
	if err != nil {
		return err
	}
	if h.Info().Model == "" {
		logx.Warnf("MCP_CONFIG is set but no model is configured, so only addon servers were started")
		maps.DeleteFunc(servers, func(_ string, s server) bool { return s.Addon == nil })
	}
	addons := false
	for _, s := range servers {
		addons = addons || s.Addon != nil
	}
	var k *keeper
	if addons { // so MCP without addons needs no state directory
		store, err := h.Store("mcp")
		var doc saved
		if err == nil {
			doc, err = loadSaved(store)
		}
		if err != nil {
			logx.Errorf("addon features are off: the pseudonyms' secret can't be kept: %v", err)
		} else {
			k = newKeeper(store, doc, servers)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.Go(func(life context.Context) { <-life.Done(); cancel() })
	if err := start(ctx, h, path, servers, k); err != nil {
		return err
	}
	if k != nil && len(k.addons) > 0 {
		// After the chat commands, so a command is never a watch's.
		h.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			for _, a := range k.addons {
				if act := a.claim(m); !act.IsZero() {
					return act
				}
			}
			return feature.Action{}
		})
		h.OnEvent(func(_ context.Context, e feature.Event) string {
			for _, a := range k.addons {
				if a.reacted(e) {
					break
				}
			}
			return ""
		})
		h.OnPress(func(ctx context.Context, p feature.Press) (string, bool) {
			for _, a := range k.addons {
				if answer, ok := a.press(ctx, p); ok {
					return answer, true
				}
			}
			return "", false
		})
		h.Go(k.run)
	}
	return nil
}

func load(path string) (map[string]server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Servers map[string]server `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		var syntax *json.SyntaxError
		var typ *json.UnmarshalTypeError
		switch {
		case errors.As(err, &syntax): // where, so a missing comma can be found
			before := data[:min(max(int(syntax.Offset)-1, 0), len(data))] // up to the bad character
			line := bytes.Count(before, []byte("\n")) + 1
			col := len(before) - bytes.LastIndexByte(before, '\n')
			return nil, fmt.Errorf("%s: line %d, column %d: %v", path, line, col, err)
		case errors.As(err, &typ) && strings.Count(typ.Field, ".") == 1 && strings.HasPrefix(typ.Field, "mcpServers."):
			name := strings.TrimPrefix(typ.Field, "mcpServers.")
			return nil, fmt.Errorf("%s: MCP server %q must be an object, not %s: is %q a setting outside its server's braces?",
				path, txt.Clamp(name, 40), typ.Value, txt.Clamp(name, 40))
		}
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	var raw struct {
		Servers map[string]map[string]json.RawMessage `json:"mcpServers"`
	}
	json.Unmarshal(data, &raw) // it decoded above, into the same shape
	for name, s := range f.Servers {
		if s.Disabled {
			delete(f.Servers, name) // not started, and not checked, so it can be parked half-written
			continue
		}
		bad := func(format string, args ...any) error {
			return fmt.Errorf("%s: MCP server %q: %s", path, txt.Clamp(name, 40), fmt.Sprintf(format, args...))
		}
		if err := unknown(raw.Servers[name], settings); err != nil {
			return nil, bad("%v", err)
		}
		var cmds map[string]map[string]json.RawMessage
		json.Unmarshal(raw.Servers[name]["chat_commands"], &cmds)
		for n, c := range cmds {
			if err := unknown(c, commandSettings); err != nil {
				return nil, bad("chat command %q: %v", txt.Clamp(n, 40), err)
			}
			cc := s.ChatCommands[n]
			if cc.Direct && s.Addon == nil {
				return nil, bad("chat command %q is direct, which needs an addon block", txt.Clamp(n, 40))
			}
			if _, set := c["timeout"]; set {
				if !cc.Direct {
					return nil, bad("chat command %q has a timeout, which only a direct command takes", txt.Clamp(n, 40))
				}
				d, err := time.ParseDuration(cc.Timeout)
				if err != nil || d < time.Second || d > maxDirectTimeout {
					return nil, bad("chat command %q: timeout %q isn't a time from 1s to 2m, such as 15s or 1m", txt.Clamp(n, 40), txt.Clamp(cc.Timeout, 40))
				}
				cc.timeout = d
				s.ChatCommands[n] = cc
			}
		}
		switch {
		case (s.URL == "") == (s.Command == ""):
			return nil, bad("needs a url or a command, not both or neither")
		case s.AllowedTools != nil && s.BlockedTools != nil:
			return nil, bad("has both allowed_tools and blocked_tools; use one")
		}
		if s.Addon != nil {
			var entry map[string]json.RawMessage
			json.Unmarshal(raw.Servers[name]["addon"], &entry)
			if err := s.Addon.check(entry); err != nil {
				return nil, bad("%v", err)
			}
			if err := checkAddonServer(name, s); err != nil {
				return nil, bad("%v", err)
			}
		}
		if err := s.expand(); err != nil {
			return nil, bad("%v", err)
		}
		f.Servers[name] = s
	}
	return f.Servers, nil
}

// unknown is an error naming the first key of entry, in order, that is
// neither one of known nor ignored, with the keys it could be.
func unknown(entry map[string]json.RawMessage, known []string) error {
	for _, k := range slices.Sorted(maps.Keys(entry)) {
		if !slices.Contains(known, k) && !slices.Contains(ignored, k) {
			return fmt.Errorf("unknown setting %q (settings are %s)", txt.Clamp(k, 40), strings.Join(known, ", "))
		}
	}
	return nil
}

// expand fills each ${NAME} in the server's url, headers, args and env with
// the bot's environment variable NAME, so a token can stay out of the file.
// One that isn't set is an error naming it: never an empty token sent.
func (s *server) expand() (err error) {
	fill := func(p *string) {
		if err != nil {
			return
		}
		*p = envPattern.ReplaceAllStringFunc(*p, func(m string) string {
			v, ok := os.LookupEnv(m[2 : len(m)-1])
			if !ok && err == nil {
				err = fmt.Errorf("%s isn't set in the bot's environment", m)
			}
			return v
		})
	}
	fill(&s.URL)
	for i := range s.Args {
		fill(&s.Args[i])
	}
	for _, m := range []map[string]string{s.Headers, s.Env} {
		for k, v := range m {
			fill(&v)
			m[k] = v
		}
	}
	return err
}

// maxInstructions caps a server's own instructions in the system prompt.
const maxInstructions = 1000

// handshake is what a server says of itself in the MCP handshake.
type handshake struct {
	instructions string
	title        string // its name for people, its title or else its name; "" if neither
	addon        bool   // it speaks signalbot's extension, at the bot's version
	subscribe    bool   // it takes resource subscriptions
}

// botInfo is what the bot tells a server of itself in the handshake.
type botInfo struct {
	version string // the bot's version, "dev" if unknown
	network string // the chat network, as Info names it, for addons
	buttons bool   // the network shows a post's buttons and keyboard, for addons
	addon   bool   // the server has an addon block: offer what addons get
	sample  bool   // the addon may ask the bot's model: offer sampling
}

// initialize performs the MCP handshake, as self, offering signalbot's
// extension with the chat network the bot is on, reply watches and, where
// the network shows them, buttons to an addon, and sampling to one that
// may ask the model, and returns what the
// server said of itself.
func initialize(ctx context.Context, c conn, self botInfo) (handshake, error) {
	caps := map[string]any{"experimental": map[string]any{
		ext.KeyAddon: ext.Capability{Version: ext.Version, Network: self.network, WatchReplies: self.addon, WatchReactions: self.addon, Buttons: self.addon && self.buttons},
	}}
	if self.sample {
		caps["sampling"] = map[string]any{}
	}
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocol,
		"capabilities":    caps,
		"clientInfo":      map[string]string{"name": "signalbot", "version": self.version},
	})
	if err != nil {
		return handshake{}, err
	}
	var r struct {
		Instructions string `json:"instructions"`
		ServerInfo   struct {
			Name  string `json:"name"`
			Title string `json:"title"`
		} `json:"serverInfo"`
		Capabilities struct {
			Experimental map[string]json.RawMessage `json:"experimental"`
			Resources    struct {
				Subscribe bool `json:"subscribe"`
			} `json:"resources"`
		} `json:"capabilities"`
	}
	json.Unmarshal(res, &r) // all optional; a bad result shows up in tools/list
	var offered ext.Capability
	addon := json.Unmarshal(r.Capabilities.Experimental[ext.KeyAddon], &offered) == nil && offered.Version == ext.Version
	return handshake{
		instructions: strings.TrimSpace(r.Instructions),
		title:        txt.SafeName(cmp.Or(r.ServerInfo.Title, r.ServerInfo.Name)),
		addon:        addon,
		subscribe:    r.Capabilities.Resources.Subscribe,
	}, c.notify(ctx, "notifications/initialized", nil)
}

// tool is a tool as a server lists it.
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema"`
	Meta        struct {
		Visibility []string `json:"io.github.robdevops.signalbot/visibility"` // nil: ["model"]
		UI         struct {
			Visibility []string `json:"visibility"` // MCP Apps'; nil: ["model", "app"]
		} `json:"ui"`
	} `json:"_meta"`
}

// forModel reports whether the tool may be offered to the model: neither
// signalbot's visibility nor MCP Apps' leaves the model out.
func (t tool) forModel() bool {
	return (t.Meta.Visibility == nil || slices.Contains(t.Meta.Visibility, "model")) &&
		(t.Meta.UI.Visibility == nil || slices.Contains(t.Meta.UI.Visibility, "model"))
}

// forBot reports whether the bot may call the tool itself: only if
// signalbot's visibility says so (MCP Apps' "app" is a UI's, not the bot's).
func (t tool) forBot() bool { return slices.Contains(t.Meta.Visibility, "bot") }

// modelName is the name the model sees for a server's tool.
func modelName(server, tool string) string {
	n := unsafeToolChars.ReplaceAllString(server, "_") + "__" + unsafeToolChars.ReplaceAllString(tool, "_")
	return n[:min(len(n), 64)]
}

// start connects to every server, in name order, and offers each one's
// tools that its allowed_tools and blocked_tools let through, and that are
// for the model, as a tool group, named by those tools and then its chat
// commands. A server that cannot be reached is logged and left out. A tool
// named in either list that a reachable server does not have, or two tools
// that end up with the same name, is an error starting with the file's
// path: a typo in blocked_tools would otherwise leave the tool offered.
// Local servers are stopped when ctx ends, and the bot waits for them.
// While the model can't call tools, only direct commands are registered;
// with no model, no tools are offered. k keeps the addons' state, nil to
// run addons as plain servers; each addon started joins it.
func start(ctx context.Context, h feature.Host, path string, servers map[string]server, k *keeper) error {
	info := h.Info()
	base := botInfo{version: cmp.Or(info.Version, "dev"), network: info.Network, buttons: info.Caps.Buttons}
	type owner struct{ server, tool string }
	taken := map[string]owner{} // the model's names for tools, and whose they are
	var cmds []string           // for the startup line
	var gates []*gate           // opened on return, so no request waits on one for good
	stateDirectory := ""        // read only with an addon, so MCP without one reads only MCP_CONFIG
	for _, s := range servers {
		if s.Addon != nil {
			stateDirectory = h.Env("STATE_DIRECTORY", "")
			break
		}
	}
	defer func() {
		for _, g := range gates {
			g.open(nil)
		}
	}()
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		cfg := servers[name]
		self := base
		if cfg.Addon != nil {
			// Sampling only where it can be answered: with a model, and
			// addon features on.
			self.addon, self.sample = true, cfg.Addon.Model != nil && info.Model != "" && k != nil
		}
		var c conn = &httpConn{url: cfg.URL, headers: cfg.Headers, client: httpx.Client(2), bot: self}
		var gt *gate
		if cfg.Command != "" {
			s := &stdioConn{name: name, cfg: cfg, bot: self, redact: h.Redact}
			if cfg.Addon != nil {
				s.prefix, s.updated = envPrefix(name), make(chan struct{}, 1)
				s.state = addonStateDir(stateDirectory, name)
			}
			if self.sample {
				// Sampling is offered from initialize on, so its requests are
				// taken from then on, and wait for the addon to be ready.
				gt = &gate{ready: make(chan struct{})}
				gates = append(gates, gt)
				s.serveRequests(gt.sample)
			}
			c = s
		}
		listed, hs, err := connect(ctx, c, self)
		if err != nil {
			gt.open(nil)
			logx.Errorf("MCP server %s unavailable, its tools are left out: %s", name, describe(err, cfg.Addon != nil))
			continue
		}
		for list, names := range map[string][]string{"allowed_tools": cfg.AllowedTools, "blocked_tools": cfg.BlockedTools} {
			for _, want := range names {
				if !slices.ContainsFunc(listed, func(t tool) bool { return t.Name == want }) {
					return fmt.Errorf("%s: MCP server %q: %s names %q, which the server doesn't have; its tools are: %s",
						path, name, list, txt.Clamp(want, 64), toolList(listed))
				}
			}
		}
		var a *addon
		switch {
		case cfg.Addon == nil:
		case !hs.addon:
			logx.Warnf("MCP server %s has an addon block but doesn't speak signalbot's addon extension (version %d), so it runs as a plain server", name, ext.Version)
		case k != nil:
			bot := map[string]bool{}
			for _, t := range listed {
				if t.forBot() {
					bot[t.Name] = true
				}
			}
			a = newAddon(name, cmp.Or(hs.title, name), *cfg.Addon, k.doc.Secret, c, bot)
			for _, t := range listed {
				a.only[t.Name] = t.forBot() && !t.forModel()
			}
			a.caps = info.Caps
			a.host, a.changed, a.dirty = h, k.changed, c.(*stdioConn).updated
			a.restore(k.doc.Addons[name])
			gt.open(a)
			k.addons = append(k.addons, a)
		}
		gt.open(nil) // a plain server: its requests are refused
		g := feature.ToolGroup{
			System:   instructions(cfg.Instructions, hs.instructions),
			Keywords: keywords(cfg.Keywords),
		}
		models := map[string]string{} // the model's name for each offered tool, by the server's
		for _, t := range listed {
			if info.Model == "" || !t.forModel() ||
				cfg.AllowedTools != nil && !slices.Contains(cfg.AllowedTools, t.Name) || slices.Contains(cfg.BlockedTools, t.Name) {
				continue
			}
			model := modelName(name, t.Name)
			if o, ok := taken[model]; ok {
				if o.server == name {
					return fmt.Errorf("%s: MCP server %q: tools %q and %q both become %q for the model; block one", path, name, o.tool, t.Name, model)
				}
				return fmt.Errorf("%s: MCP server %q: tool %q becomes %q for the model, as does server %q's tool %q; block one",
					path, name, t.Name, model, o.server, o.tool)
			}
			taken[model], models[t.Name] = owner{name, t.Name}, model
			g.Names = append(g.Names, t.Name)
			g.Tools = append(g.Tools, feature.Tool{Name: model, Description: t.Description, Schema: t.Schema,
				Run: func(ctx context.Context, args string) (string, []feature.Picture, error) {
					return call(ctx, c, t.Name, args)
				}})
		}
		label := name
		if !strings.Contains(strings.ToLower(name), "mcp") {
			label += " MCP" // so the intro and the log show it is an MCP server
		}
		if len(g.Names) > 0 { // before its commands join them: one line per server
			logx.Noticef("%s: %s", label, strings.Join(g.Names, ", "))
		}
		if a != nil {
			logx.Noticef("%s: an addon, with tools for the bot: %s", label, cmp.Or(strings.Join(slices.Sorted(maps.Keys(a.bot)), ", "), "none"))
		}
		var own []string // its chat commands, as "/name"
		for _, n := range slices.Sorted(maps.Keys(cfg.ChatCommands)) {
			cc := cfg.ChatCommands[n]
			if !cc.Direct && !info.CallsTools {
				continue // a command would force a tool the model can't call
			}
			if cmd := command(h, name, n, cc, models, a); cmd != "" {
				own = append(own, cmd)
			}
		}
		slices.Sort(own)
		cmds = append(cmds, own...)
		switch {
		case len(g.Tools) > 0:
			all := strings.Join(append(g.Names, own...), ", ")
			g.Listing = name + ": " + all
			g.Names = []string{"**" + label + "**: " + all} // grouped by server in {tools}, in Markdown
			h.OfferTools(g)
		case len(own) > 0: // no group to list them in
			var bare []string
			for _, cmd := range own {
				bare = append(bare, strings.TrimPrefix(cmd, "/"))
			}
			h.ListCommands(label, bare...)
		}
		if a != nil {
			c.(*stdioConn).ready = a.ready
			a.ready(ctx, hs)
			h.Go(a.work)
			h.Go(a.readOutbox)
		}
		if s, ok := c.(*stdioConn); ok {
			h.Go(func(context.Context) { s.supervise(ctx) })
		}
	}
	if len(cmds) > 0 {
		logx.Noticef("chat commands: %s", strings.Join(cmds, ", "))
	}
	return nil
}

// describe is err for the log: for an addon, only its code, never the
// server's own text.
func describe(err error, addon bool) string {
	if addon {
		return errCode(err)
	}
	return err.Error()
}

// toolList names a server's tools, the first 20 and then "…".
func toolList(tools []tool) string {
	var names []string
	for _, t := range tools[:min(len(tools), 20)] {
		names = append(names, txt.Clamp(t.Name, 64))
	}
	if len(tools) > 20 {
		names = append(names, "…")
	}
	return cmp.Or(strings.Join(names, ", "), "none")
}

// instructions is a server's system-prompt line: its config's
// "instructions" if set (even to ""), otherwise the server's own, capped.
func instructions(cfg *string, own string) string {
	if cfg != nil {
		return strings.TrimSpace(*cfg)
	}
	return txt.Clamp(own, maxInstructions)
}

// keywords is a pattern matching any of words as a whole word, ignoring
// capitals; nil without words.
func keywords(words []string) *regexp.Regexp {
	var alts []string
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			alts = append(alts, regexp.QuoteMeta(w))
		}
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`)
}

// command registers chat command n of a server on h and returns it as
// "/name". A direct one calls a tool of a, the server as an addon, that the
// bot may call; any other has the model call a tool it is offered. One that
// can't be used (such a tool missing, or a bad or taken name) is logged and
// skipped, returning "".
func command(h feature.Host, server, n string, c chatCommand, models map[string]string, a *addon) string {
	name := strings.ToLower(strings.TrimPrefix(n, "/"))
	model, offered := models[c.Tool]
	switch {
	case c.Direct && a == nil:
		logx.Warnf("MCP server %s: chat command /%s is direct, but the server's addon features are off; skipped", server, txt.Clamp(name, 40))
		return ""
	case c.Direct && !a.bot[c.Tool]:
		logx.Warnf("MCP server %s: chat command /%s names tool %q, which the bot may not call; skipped", server, txt.Clamp(name, 40), txt.Clamp(c.Tool, 64))
		return ""
	case !c.Direct && !offered:
		logx.Warnf("MCP server %s: chat command /%s names tool %q, which isn't offered; skipped", server, txt.Clamp(name, 40), txt.Clamp(c.Tool, 64))
		return ""
	}
	bare := "Put what you want after it, like /" + name + " <your request>."
	if ex := strings.TrimRight(strings.TrimSpace(c.Example), "."); ex != "" {
		bare += " Try: " + ex + "."
	}
	if !c.Direct {
		bare += " Or just ask me in plain words."
	}
	err := h.Command(name, func(ctx context.Context, m feature.Message, args string) feature.Action {
		switch {
		case c.RequireArg && args == "":
			return feature.Action{Answer: bare}
		case c.Direct:
			return a.direct(ctx, m, name, c, args)
		}
		return feature.Action{ToolChoice: model}
	})
	if err != nil {
		logx.Warnf("MCP server %s: chat command skipped: %v", server, err)
		return ""
	}
	if c.Direct {
		a.cmds[name] = true
	}
	return "/" + name
}

// connect starts the server, initializes it, and lists its tools by name,
// all of them, whoever they are for.
func connect(ctx context.Context, c conn, self botInfo) ([]tool, handshake, error) {
	ctx2, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if s, ok := c.(*stdioConn); ok {
		if err := s.start(ctx); err != nil {
			return nil, handshake{}, err
		}
	}
	hs, err := initialize(ctx2, c, self)
	if err != nil {
		return nil, handshake{}, err
	}
	var tools []tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.call(ctx2, "tools/list", params)
		if err != nil {
			return nil, handshake{}, err
		}
		var page struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, handshake{}, fmt.Errorf("tools/list: %v", err)
		}
		tools = append(tools, page.Tools...)
		if cursor = page.NextCursor; cursor == "" {
			slices.SortFunc(tools, func(a, b tool) int { return strings.Compare(a.Name, b.Name) })
			return slices.CompactFunc(tools, func(a, b tool) bool { return a.Name == b.Name }), hs, nil
		}
	}
}

// call runs a server's tool with the JSON arguments the model wrote, and
// returns the text to give back to the model and any pictures to send to
// the chat. err is for the log only, and never holds the arguments or the
// result.
func call(ctx context.Context, c conn, name, arguments string) (string, []feature.Picture, error) {
	args := map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if json.Unmarshal([]byte(arguments), &args) != nil {
			return "error: the arguments were not a JSON object", nil, errors.New("invalid arguments")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		if re, ok := errors.AsType[*rpcError](err); ok {
			return "error: " + txt.Clamp(re.Message, 500), nil, fmt.Errorf("rpc error %d", re.Code)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Sprintf("error: the tool didn't answer within %ds; try a narrower request or later", int(callTimeout.Seconds())), nil, err
		}
		return "error: the tool could not be reached", nil, err
	}
	var r toolResult
	if json.Unmarshal(res, &r) != nil {
		return "error: the tool sent a malformed result", nil, errors.New("malformed result")
	}
	var parts []string
	var pics []feature.Picture
	for _, p := range r.Content {
		if p.Type == "text" {
			parts = append(parts, p.Text)
			continue
		}
		data, err := base64.StdEncoding.DecodeString(p.Data)
		if err == nil {
			data, err = img.Safe(data) // its metadata, such as where a photo was taken, never sent
		}
		if p.Type == "image" && err == nil && img.Sendable(data) && len(pics) < feature.MaxPictures {
			pics = append(pics, feature.Picture{Data: data})
			parts = append(parts, "[image sent to the chat]")
		} else {
			parts = append(parts, "[non-text content omitted]")
		}
	}
	out := txt.Clamp(strings.Join(parts, "\n"), maxResult)
	if r.IsError {
		return "error: " + out, nil, errors.New("the tool reported an error")
	}
	return out, pics, nil
}

// toolResult is a tools/call result. An addon's _meta may make a watch,
// for a direct command, or ask for a reaction, for a watch's call.
type toolResult struct {
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Data     string `json:"data"` // base64, for an image
		MimeType string `json:"mimeType"`
	} `json:"content"`
	IsError bool `json:"isError"`
	Meta    struct {
		Watch *ext.Watch `json:"io.github.robdevops.signalbot/watch"`
		React string     `json:"io.github.robdevops.signalbot/react"` // an emoji to put on the message
	} `json:"_meta"`
}
