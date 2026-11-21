package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/png"
	"io"
	"maps"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Group IDs as Signal names them, and a Telegram one.
var (
	groupA   = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	groupB   = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	telegram = "-1001234567890"
)

// addonTools is what the fake addon lists: who each tool is for, by its
// _meta.
var addonTools = map[string]string{
	"play":    `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"both":    `{"io.github.robdevops.signalbot/visibility":["model","bot"]}`,
	"plain":   ``,
	"app":     `{"ui":{"visibility":["app"]}}`,
	"app_too": `{"ui":{"visibility":["model","app"]},"io.github.robdevops.signalbot/visibility":["model","bot"]}`,
	"context": `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"pics":    `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"big":     `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"fail":    `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"rpcfail": `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"hang":    `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"huge":    `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"env":     `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"empty":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"spawn":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"round":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"guess":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"queue":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"epoch":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
	"crash":   `{"io.github.robdevops.signalbot/visibility":["bot"]}`,
}

// TestAddonProcess is not a real test: run as a subprocess with the
// argument "addon" (or "plain", which doesn't speak the extension, or
// "sleep", a process it starts that only sleeps), it is a stdio MCP server
// with addonTools and an outbox (see fakeOutbox). It answers calls at
// once, each on its own, echoes every line it gets to stderr, and exits
// when its stdin closes.
func TestAddonProcess(t *testing.T) {
	mode := flag.Arg(0)
	switch mode {
	case "addon", "plain":
	case "sleep":
		time.Sleep(time.Hour)
		os.Exit(0)
	default:
		return
	}
	var out sync.Mutex
	reply := func(id int64, result string) {
		out.Lock()
		defer out.Unlock()
		fmt.Printf(`{"jsonrpc":"2.0","id":%d,%s}`+"\n", id, result)
	}
	box := &fakeOutbox{nonce: "a", notify: func() {
		out.Lock()
		defer out.Unlock()
		fmt.Println(`{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"signalbot://outbox/x"}}`)
	}}
	text := func(s string) string {
		b, _ := json.Marshal(s)
		return `"result":{"content":[{"type":"text","text":` + string(b) + `}]}`
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		fmt.Fprintln(os.Stderr, sc.Text())
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
				Meta      json.RawMessage   `json:"_meta"`
				URI       string            `json:"uri"`
				Caps      struct {
					Experimental map[string]struct{ Version int } `json:"experimental"`
				} `json:"capabilities"`
			} `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &req)
		if req.Method == "notifications/cancelled" {
			fakeLog("cancelled " + string(sc.Bytes()))
		}
		if req.ID == nil {
			continue
		}
		id, p := *req.ID, req.Params
		switch req.Method {
		case "initialize":
			caps := `{"tools":{}}`
			if mode == "addon" && p.Caps.Experimental[ext.KeyAddon].Version == 1 { // only if offered
				caps = `{"tools":{},"resources":{"subscribe":true},"experimental":{"io.github.robdevops.signalbot/addon":{"version":1}}}`
			}
			reply(id, `"result":{"protocolVersion":"`+protocol+`","capabilities":`+caps+`,"serverInfo":{"name":"game","title":"Game","version":"1"}}`)
		case "tools/list":
			var list []string
			for _, n := range slices.Sorted(maps.Keys(addonTools)) {
				t := `{"name":"` + n + `","inputSchema":{"type":"object"}`
				if m := addonTools[n]; m != "" {
					t += `,"_meta":` + m
				}
				list = append(list, t+"}")
			}
			reply(id, `"result":{"tools":[`+strings.Join(list, ",")+`]}`)
		case "tools/call":
			go func() {
				switch p.Name {
				case "play":
					var meta map[string]ext.Context
					json.Unmarshal(p.Meta, &meta)
					reply(id, text("you said "+p.Arguments["args"]+"\n{"+meta[ext.KeyContext].Sender+"} plays"))
				case "context":
					reply(id, text(string(p.Meta)))
				case "pics":
					pic := `{"type":"image","mimeType":"image/png","data":"` + tinyPNG() + `"}`
					reply(id, `"result":{"content":[`+pic+`,{"type":"image","data":"eA=="},`+pic+`,`+pic+`]}`)
				case "big":
					reply(id, `"result":{"content":[{"type":"image","data":"`+tinyPNG()+`"},{"type":"image","data":"`+noisePNG()+`"},{"type":"text","text":"two"}]}`)
				case "fail":
					reply(id, `"result":{"content":[{"type":"text","text":"Alice's secret broke"}],"isError":true}`)
				case "rpcfail":
					reply(id, `"error":{"code":-32000,"message":"Bob's secret broke"}`)
				case "hang":
				case "huge":
					out.Lock()
					fmt.Println(strings.Repeat("x", 5<<20))
					out.Unlock()
				case "env":
					var names []string
					for _, kv := range os.Environ() {
						k, _, _ := strings.Cut(kv, "=")
						names = append(names, k)
					}
					slices.Sort(names)
					reply(id, text(strings.Join(names, ",")))
				case "empty":
					reply(id, `"result":{"content":[]}`)
				case "round", "guess", "queue", "epoch", "crash":
					reply(id, box.call(p.Name, p.Arguments, p.Meta))
				case "spawn":
					cmd := exec.Command(os.Args[0], "-test.run=^TestAddonProcess$", "sleep")
					cmd.Start()
					reply(id, text(strconv.Itoa(cmd.Process.Pid)))
				default:
					reply(id, text("called "+p.Name))
				}
			}()
		case "resources/subscribe":
			fakeLog("subscribed")
			reply(id, `"result":{}`)
		case "resources/read":
			go func() { reply(id, box.read(p.URI)) }()
		default:
			reply(id, `"result":{}`)
		}
	}
	os.Exit(0)
}

// noisePNG is a PNG of noise, over 2 MB.
func noisePNG() string {
	im := image.NewRGBA(image.Rect(0, 0, 800, 800))
	r := rand.New(rand.NewPCG(1, 2))
	for i := range im.Pix {
		im.Pix[i] = uint8(r.Uint32())
	}
	var buf bytes.Buffer
	png.Encode(&buf, im)
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// addonServer is a config entry for the fake addon, in mode, with addon
// as its addon block and more settings after it.
func addonServer(mode, addon, more string) string {
	self, _ := json.Marshal(os.Args[0])
	return `{"command":` + string(self) + `,"args":["-test.run=^TestAddonProcess$","` + mode + `"],"addon":` + addon + more + `}`
}

// direct is the chat_commands setting for /play, and /ctx, /pics and the
// rest, each calling the tool of its name directly.
const direct = `,"chat_commands":{"play":{"tool":"play","direct":true,"example":"/play 3m"},"ctx":{"tool":"context","direct":true},` +
	`"pics":{"tool":"pics","direct":true},"big":{"tool":"big","direct":true},"fail":{"tool":"fail","direct":true},` +
	`"rpcfail":{"tool":"rpcfail","direct":true},"hang":{"tool":"hang","direct":true},"huge":{"tool":"huge","direct":true},` +
	`"env":{"tool":"env","direct":true},"empty":{"tool":"empty","direct":true},"spawn":{"tool":"spawn","direct":true},` +
	`"app":{"tool":"app","direct":true},"plain":{"tool":"plain","direct":true},"both":{"tool":"both"}}`

// startAddon registers config on a host with a model that calls tools and
// a state directory of its own, waits until the servers are up, and stops
// them when the test ends.
func startAddon(t *testing.T, config string, before ...func(*featuretest.Host)) (*featuretest.Host, *logx.Buffer) {
	t.Helper()
	dir := t.TempDir()
	h, log, err := register(t, config, append([]func(*featuretest.Host){func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = dir }}, before...)...)
	if err != nil {
		t.Fatal(err)
	}
	return h, log
}

// in is an addressed message from Alice, in chat.
func in(chat feature.Chat, text string) feature.Message {
	return feature.Message{Chat: chat, Sender: "alice-id", Name: "Alice", Text: text, Timestamp: 1700000000000, Addressed: true}
}

var (
	inA  = feature.Chat{GroupID: groupA}
	inB  = feature.Chat{GroupID: groupB}
	inDM = feature.Chat{User: "alice-id"}
)

func TestEnvPrefix(t *testing.T) {
	for name, want := range map[string]string{"geo": "GEO_", "my-addon.v2": "MY_ADDON_V2_", "Ünï": "_N__"} {
		if got := envPrefix(name); got != want {
			t.Errorf("envPrefix(%q) = %q, want %q", name, got, want)
		}
	}
}

// coreSettings is docs/CONFIGURATION.md's table, so no addon's prefix
// starts one of the bot's own settings.
func TestCoreSettingsAreTheDocs(t *testing.T) {
	doc, err := os.ReadFile("../../../docs/CONFIGURATION.md")
	if err != nil {
		t.Fatal(err)
	}
	var documented []string
	for _, m := range regexp.MustCompile("(?m)^\\| `([A-Z0-9_]+)` \\|").FindAllStringSubmatch(string(doc), -1) {
		documented = append(documented, m[1])
	}
	want := slices.Sorted(slices.Values(append(documented, "STATE_DIRECTORY")))
	if got := slices.Sorted(slices.Values(coreSettings)); !slices.Equal(slices.Compact(want), got) {
		t.Fatalf("coreSettings %v,\ndocs %v", got, slices.Compact(want))
	}
}

func TestAddonLoad(t *testing.T) {
	s, err := load(writeConfig(t, `{"mcpServers":{"game":`+addonServer("addon", `{"chats":["`+groupA+`","`+telegram+`"],"dms":true}`, direct)+`,`+
		`"all":`+addonServer("addon", `{"chats":["all"],"watch_max":"30m","rate":{"per_minute":6}}`, "")+`}}`))
	if err != nil {
		t.Fatal(err)
	}
	if a := s["game"].Addon; a == nil || !a.DMs || len(a.Chats) != 2 || a.watchMax != time.Hour || a.Rate != (rateConfig{20, 5}) || !s["game"].ChatCommands["play"].Direct {
		t.Fatalf("game: %+v", s["game"])
	}
	if a := s["all"].Addon; a.watchMax != 30*time.Minute || a.Rate != (rateConfig{6, 5}) {
		t.Fatalf("all: %+v", a)
	}
	for bad, want := range map[string]string{
		`{"mcpServers":{"geo":{"url":"https://x","addon":{}}}}`:                                             `: MCP server "geo": an addon needs a command: addons run only as local servers`,
		`{"mcpServers":{"geo":{"command":"x","env":{"A":"b"},"addon":{}}}}`:                                 `: MCP server "geo": an addon server takes no env block: put its settings in the bot's environment, starting with GEO_, and it gets them`,
		`{"mcpServers":{"model":{"command":"x","addon":{}}}}`:                                               `: MCP server "model": an addon's settings start with MODEL_, as the bot's own MODEL_API_KEY does; rename the server`,
		`{"mcpServers":{"Daily":{"command":"x","addon":{}}}}`:                                               `: MCP server "Daily": an addon's settings start with DAILY_, as the bot's own DAILY_BUDGET does; rename the server`,
		`{"mcpServers":{"geo":{"command":"x","chat_commands":{"geo":{"tool":"geo","direct":true}}}}}`:       `: MCP server "geo": chat command "geo" is direct, which needs an addon block`,
		`{"mcpServers":{"geo":{"command":"x","addon":{"chat":["all"]}}}}`:                                   `: MCP server "geo": addon: unknown setting "chat" (settings are chats, dms, post, react, watch, watch_max, names, global_ids, rate)`,
		`{"mcpServers":{"geo":{"command":"x","addon":{"rate":{"perMinute":1}}}}}`:                           `: MCP server "geo": addon rate: unknown setting "perMinute" (settings are per_minute, burst)`,
		`{"mcpServers":{"geo":{"command":"x","addon":{"chats":["Family"]}}}}`:                               `: MCP server "geo": addon chats: "Family" isn't a group ID; copy the group's ID as Signal or Telegram shows it, or use "all"`,
		`{"mcpServers":{"geo":{"command":"x","addon":{"chats":["all","` + groupA + `"]}}}}`:                 `: MCP server "geo": addon chats: "all" goes alone`,
		`{"mcpServers":{"geo":{"command":"x","addon":{"watch_max":"25h"}}}}`:                                `: MCP server "geo": addon watch_max: "25h" isn't a time from 1s to 24h, such as 1h or 30m`,
		`{"mcpServers":{"geo":{"command":"x","addon":{"rate":{"burst":-1}}}}}`:                              `: MCP server "geo": addon rate: per_minute and burst can't be below 0`,
		`{"mcpServers":{"geo":{"command":"x","chat_commands":{"g":{"tool":"g","drect":true}},"addon":{}}}}`: `: MCP server "geo": chat command "g": unknown setting "drect" (settings are tool, require_arg, example, direct, timeout)`,
	} {
		p := writeConfig(t, bad)
		if _, err := load(p); err == nil || err.Error() != p+want {
			t.Errorf("%s:\n got %v\nwant %s", bad, err, want)
		}
	}
}

// An addon gets PATH, HOME, the time zone and proxy settings, and the
// bot's variables with its prefix, never the bot's own; it is offered
// signalbot's extension in the handshake.
func TestAddonEnv(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "secret-key")
	t.Setenv("GAME_TOKEN", "t")
	t.Setenv("GAMES_X", "x")
	for _, name := range shared {
		t.Setenv(name, "x")
	}
	h, _ := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`, direct)+`}}`)
	want := strings.Join(slices.Sorted(slices.Values(append([]string{"GAME_TOKEN", "HOME", "PATH"}, shared...))), ",")
	if got := h.Message(context.Background(), in(inDM, "/env")); got.Answer != want {
		t.Fatalf("env %+v", got)
	}
}

// What an addon writes to stderr is counted, and what was counted since the
// last count logged is logged when it exits.
func TestAddonStderr(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	c := &stdioConn{name: "game", prefix: "GAME_"}
	c.readStderr(io.NopCloser(strings.NewReader("started\npanic: Alice\ngoroutine 1\n")))
	if got, want := log.String(), "<6>addon game wrote 1 line (7 bytes) to stderr\n<6>addon game wrote 2 lines (23 bytes) to stderr\n"; got != want {
		t.Fatalf("log %q", got)
	}
}

// Tools that aren't for the model are never offered to it; a direct
// command calls a tool for the bot, with the message's context, and its
// placeholders filled in; only in the chats the addon is allowed.
func TestDirect(t *testing.T) {
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"chats":["`+groupA+`"]}`, direct)+`}}`)
	if gs := h.ToolGroups(); len(gs) != 1 || gs[0].Listing != "game: app_too, both, plain, /big, /both, /ctx, /empty, /env, /fail, /hang, /huge, /pics, /play, /rpcfail, /spawn" {
		t.Fatalf("groups %+v", gs)
	}
	if got := strings.Join(slices.Sorted(maps.Keys(tools(h))), ","); got != "game__app_too,game__both,game__plain" {
		t.Fatalf("offered %s", got)
	}
	for _, want := range []string{
		"<5>game MCP: app_too, both, plain\n",
		"<5>game MCP: an addon, with tools for the bot: app_too, big, both, context, crash, empty, env, epoch, fail, guess, hang, huge, pics, play, queue, round, rpcfail, spawn\n",
		`<4>MCP server game: chat command /app names tool "app", which the bot may not call; skipped`,
		`<4>MCP server game: chat command /plain names tool "plain", which the bot may not call; skipped`,
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q, want %q", log.String(), want)
		}
	}
	ctx := context.Background()
	if got := h.Message(ctx, in(inA, "/play 3m")); got.Answer != "you said 3m\n@Alice plays" || got.Done || len(got.Pictures) != 0 {
		t.Fatalf("play: %+v", got)
	}
	if got := h.Message(ctx, in(inA, "/both")); got.ToolChoice != "game__both" {
		t.Fatalf("a command for the model: %+v", got)
	}
	for _, chat := range []feature.Chat{inB, inDM} {
		if got := h.Message(ctx, in(chat, "/play")); got.Answer != "/play isn't available in this chat." {
			t.Errorf("%v: %+v", chat, got)
		}
	}
	got := h.Message(ctx, in(inA, "/ctx"))
	var meta map[string]ext.Context
	if err := json.Unmarshal([]byte(got.Answer), &meta); err != nil {
		t.Fatalf("ctx %q: %v", got.Answer, err)
	}
	c := meta[ext.KeyContext]
	id := regexp.MustCompile(`^[cur]_[a-z2-7]{26}$`)
	if !id.MatchString(c.Chat) || !id.MatchString(c.Sender) || !id.MatchString(c.Ref) || c.Chat[0] != 'c' || c.Sender[0] != 'u' || c.Ref[0] != 'r' ||
		!c.Addressed || !c.Group || c.Edited != 0 || c.Name != "" || time.Since(time.Unix(c.Sent, 0)) > time.Minute ||
		time.Until(time.Unix(c.Deadline, 0)) > directTimeout || time.Until(time.Unix(c.Deadline, 0)) < directTimeout-5*time.Second {
		t.Fatalf("context %+v", c)
	}
	if strings.Contains(got.Answer, "alice-id") || strings.Contains(got.Answer, groupA) || strings.Contains(got.Answer, "Alice") {
		t.Fatalf("a real name or ID reached the addon: %s", got.Answer)
	}
	// Each call is logged by tool, outcome and size only.
	if !regexp.MustCompile(`<6>MCP server game: /play called play: ok in \d+ms, \d+ bytes\n`).MatchString(log.String()) {
		t.Errorf("log %q", log.String())
	}
}

// With no model, only addons start: no tools are offered, and direct
// commands, listed by themselves, still work.
func TestDirectNoModel(t *testing.T) {
	gone := writeConfig(t, "")
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`,
		`,"chat_commands":{"play":{"tool":"play","direct":true},"both":{"tool":"both"}}`)+`,"other":{"command":"`+gone+`"}}}`,
		func(h *featuretest.Host) { h.Bot = feature.Info{} })
	if gs := h.ToolGroups(); len(gs) != 1 || len(gs[0].Tools) != 0 || !slices.Equal(gs[0].Names, []string{"**game MCP**: /play"}) {
		t.Fatalf("groups %+v", gs)
	}
	if got := h.Message(context.Background(), in(inDM, "/play")); got.Answer != "you said \n@Alice plays" {
		t.Fatalf("play: %+v", got)
	}
	if l := log.String(); !strings.HasPrefix(l, "<4>MCP_CONFIG is set but no model is configured, so only addon servers were started\n") || strings.Contains(l, "other") {
		t.Fatalf("log %q", l)
	}
}

// A direct command's images are made safe and capped; a result with
// nothing is done with, not sent to the model; a failure is told in a
// line for people, its text never shown or logged; a call that takes too
// long is given up; past two at once, the addon is busy.
func TestDirectResults(t *testing.T) {
	defer func(d time.Duration) { directTimeout = d }(directTimeout)
	directTimeout = 3 * time.Second
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`, direct)+`}}`)
	ctx := context.Background()
	if got := h.Message(ctx, in(inDM, "/pics")); got.Answer != "" || len(got.Pictures) != 2 || got.Done {
		t.Fatalf("pics: %+v", got)
	}
	if got := h.Message(ctx, in(inDM, "/big")); got.Answer != "two" || len(got.Pictures) != 1 {
		t.Fatalf("big: %d pictures, %q", len(got.Pictures), got.Answer)
	}
	if !strings.Contains(log.String(), "<4>MCP server game: 1 images left out: not images, or past 2 images or 2 MB\n") ||
		!strings.Contains(log.String(), "<4>MCP server game: 2 images left out") {
		t.Errorf("log %q", log.String())
	}
	if got := h.Message(ctx, in(inDM, "/empty")); !got.Done || got.Answer != "" {
		t.Fatalf("empty: %+v", got)
	}
	for _, cmd := range []string{"/fail", "/rpcfail"} {
		if got := h.Message(ctx, in(inDM, cmd)); got.Answer != "Game couldn't do that just now." {
			t.Errorf("%s: %+v", cmd, got)
		}
	}
	answers := make(chan string, 3)
	for range 3 {
		go func() { answers <- h.Message(ctx, in(inDM, "/hang")).Answer }()
	}
	var got []string
	for range 3 {
		got = append(got, <-answers)
	}
	slices.Sort(got)
	if want := []string{"Game couldn't do that just now.", "Game couldn't do that just now.", "Game is busy, try again in a moment."}; !slices.Equal(got, want) {
		t.Errorf("hangs: %q", got)
	}
	l := log.String()
	for _, want := range []string{"/fail called fail: an error result", "/rpcfail called rpcfail: rpc error -32000", "/hang called hang: no answer in time",
		"<4>MCP server game: /hang refused: 2 calls already in flight\n"} {
		if !strings.Contains(l, want) {
			t.Errorf("log %q, want %q", l, want)
		}
	}
	if strings.Contains(l, "secret") {
		t.Fatalf("the server's error text was logged: %q", l)
	}
}

// A direct command's timeout replaces directTimeout for its calls, and a
// call past it is cancelled, so the addon can stop work nobody will see.
func TestDirectTimeout(t *testing.T) {
	t.Setenv("GAME_LOG", filepath.Join(t.TempDir(), "game.log"))
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`,
		`,"chat_commands":{"hang":{"tool":"hang","direct":true,"timeout":"1s"}}`)+`}}`)
	start := time.Now()
	if got := h.Message(context.Background(), in(inDM, "/hang")); got.Answer != "Game couldn't do that just now." {
		t.Fatalf("hang: %+v", got)
	}
	if took := time.Since(start); took < time.Second || took >= directTimeout {
		t.Errorf("took %v, want 1s", took)
	}
	if !strings.Contains(log.String(), "/hang called hang: no answer in time") {
		t.Errorf("log %q", log.String())
	}
	eventually(t, "a cancellation", func() bool { return len(gameLog(t, "cancelled ")) == 1 })
	if got := gameLog(t, "cancelled ")[0]; !regexp.MustCompile(`"params":\{"requestId":\d+\}`).MatchString(got) {
		t.Errorf("cancelled %s", got)
	}
}

// A require_arg direct command answers a bare one with its example, not
// pointing to the model.
func TestDirectRequireArg(t *testing.T) {
	h, _ := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`,
		`,"chat_commands":{"play":{"tool":"play","direct":true,"require_arg":true,"example":"/play 3m"}}`)+`}}`)
	if got := h.Message(context.Background(), in(inDM, "/play")); got.Answer != "Put what you want after it, like /play <your request>. Try: /play 3m." {
		t.Fatalf("bare: %+v", got)
	}
}

// A server with an addon block that doesn't speak the extension runs as a
// plain server: its direct commands are skipped, and its tools for the
// model are offered.
func TestNotAnAddon(t *testing.T) {
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("plain", `{"dms":true}`, direct)+`}}`)
	for _, want := range []string{
		"<4>MCP server game has an addon block but doesn't speak signalbot's addon extension (version 1), so it runs as a plain server\n",
		"<4>MCP server game: chat command /play is direct, but the server's addon features are off; skipped\n",
		"<5>chat commands: /both\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q, want %q", log.String(), want)
		}
	}
	if got := h.Message(context.Background(), in(inDM, "/play")); !got.IsZero() {
		t.Fatalf("play: %+v", got)
	}
}

// Pseudonyms are per chat unless global_ids, differ between servers, and
// are made from a secret kept in the "mcp" store, so they survive a
// restart. A secret that can't be kept turns addons off.
func TestPseudonyms(t *testing.T) {
	secret := bytes.Repeat([]byte{7}, secretSize)
	a := newAddon("game", "Game", addonConfig{}, secret, nil, nil)
	g := newAddon("game", "Game", addonConfig{GlobalIDs: true}, secret, nil, nil)
	other := newAddon("game2", "Game", addonConfig{}, secret, nil, nil)
	if a.userID(inA, "alice-id") == a.userID(inB, "alice-id") || a.userID(inA, "alice-id") == a.userID(inA, "bob-id") ||
		g.userID(inA, "alice-id") != g.userID(inB, "alice-id") || a.chatID(inA) == other.chatID(inA) || a.chatID(inA) == a.chatID(inB) ||
		a.chatID(inA) != newAddon("game", "", addonConfig{}, secret, nil, nil).chatID(inA) {
		t.Fatal("pseudonyms don't keep apart what they should")
	}

	dir := t.TempDir()
	cfg := `{"mcpServers":{"game":` + addonServer("addon", `{"dms":true}`, direct) + `}}`
	chatIDs := func() string {
		h, _, err := register(t, cfg, func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = dir })
		if err != nil {
			t.Fatal(err)
		}
		defer h.Stop()
		var meta map[string]ext.Context
		json.Unmarshal([]byte(h.Message(context.Background(), in(inDM, "/ctx")).Answer), &meta)
		return meta[ext.KeyContext].Chat
	}
	first := chatIDs()
	if first == "" || chatIDs() != first {
		t.Fatal("pseudonyms changed with a restart")
	}
	var s saved
	data, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil || json.Unmarshal(data, &s) != nil || len(s.Secret) != secretSize {
		t.Fatalf("saved %s, %v", data, err)
	}

	// mcp.json can't be read: addons fail closed.
	broken := t.TempDir()
	os.Mkdir(filepath.Join(broken, "mcp.json"), 0o700)
	h, log, err := register(t, cfg, func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = broken })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "<3>addon features are off: the pseudonyms' secret can't be kept: ") ||
		!strings.Contains(log.String(), "/play is direct, but the server's addon features are off; skipped") {
		t.Fatalf("log %q", log.String())
	}
	if got := h.Message(context.Background(), in(inDM, "/play")); !got.IsZero() {
		t.Fatalf("play with addons off: %+v", got)
	}
	// So does a state directory the host can't open.
	_, log, err = register(t, cfg, func(h *featuretest.Host) { h.StoreErr = fmt.Errorf("no state directory") })
	if err != nil || !strings.Contains(log.String(), "<3>addon features are off: the pseudonyms' secret can't be kept: no state directory\n") {
		t.Fatalf("log %q, %v", log.String(), err)
	}
}

// A placeholder becomes the name its sender last had in that chat, as the
// addon saw them, else "someone"; names expire.
func TestFill(t *testing.T) {
	a := newAddon("game", "Game", addonConfig{}, bytes.Repeat([]byte{7}, secretSize), nil, nil)
	now := time.Now()
	a.now = func() time.Time { return now }
	alice, bob := a.userID(inA, "alice-id"), a.userID(inA, "bob-id")
	a.context(in(inA, "/play"), now)
	bobs := in(inA, "/play")
	bobs.Sender, bobs.Name = "bob-id", "Bob"
	a.context(bobs, now)
	if got := a.fill(inA, "{"+alice+"} beat {"+bob+"} and {"+a.userID(inA, "x")+"}; {u_x}"); got != "@Alice beat @Bob and someone; {u_x}" {
		t.Fatalf("filled %q", got)
	}
	if got := a.fill(inB, "{"+alice+"}"); got != "someone" {
		t.Fatalf("another chat: %q", got)
	}
	now = now.Add(nameLife + time.Second)
	if got := a.fill(inA, "{"+alice+"}"); got != "someone" {
		t.Fatalf("expired: %q", got)
	}
	n := a.context(feature.Message{Chat: inA, Sender: "alice-id", Name: "Alice", Edited: 5}, now)
	if n.Edited != n.Sent || n.Name != "" {
		t.Fatalf("edit %+v", n)
	}
	a.cfg.Names = true
	if n := a.context(in(inA, "/play"), now); n.Name != "Alice" || n.Edited != 0 {
		t.Fatalf("names %+v", n)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.refs) != 2 { // the first two expired
		t.Fatalf("%d refs", len(a.refs))
	}
}

// An addon's stderr is only counted, never logged: it may echo chat text.
// A line over 4 MiB on stdout gets it restarted.
func TestAddonOutput(t *testing.T) {
	defer func(d time.Duration) { restartDelay = d }(restartDelay)
	restartDelay = 10 * time.Millisecond
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`, direct)+`}}`)
	ctx := context.Background()
	if got := h.Message(ctx, in(inDM, "/play my address is 1 Long Rd")); got.Answer != "you said my address is 1 Long Rd\n@Alice plays" {
		t.Fatalf("play: %+v", got)
	}
	if got := h.Message(ctx, in(inDM, "/huge")); got.Answer != "Game couldn't do that just now." {
		t.Fatalf("huge: %+v", got)
	}
	deadline := time.Now().Add(10 * time.Second)
	for h.Message(ctx, in(inDM, "/play")).Answer != "you said \n@Alice plays" {
		if time.Now().After(deadline) {
			t.Fatalf("not restarted: %s", log.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	l := log.String()
	if !strings.Contains(l, "<4>MCP server game wrote a line over 4 MiB, so it is stopped and restarted\n") || !strings.Contains(l, "<4>MCP server game exited; restarting in") ||
		!regexp.MustCompile(`<6>addon game wrote \d+ lines? \(\d+ bytes\) to stderr\n`).MatchString(l) {
		t.Fatalf("log %q", l)
	}
	if strings.Contains(l, "Long Rd") || strings.Contains(l, "initialize") {
		t.Fatalf("stderr was logged: %q", l)
	}
}

// When the bot stops, so does everything the addon started.
func TestAddonStops(t *testing.T) {
	h, _ := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`, direct)+`}}`)
	pid, err := strconv.Atoi(h.Message(context.Background(), in(inDM, "/spawn")).Answer)
	if err != nil {
		t.Fatal(err)
	}
	if !alive(pid) {
		t.Fatal("not started")
	}
	h.Stop()
	for deadline := time.Now().Add(10 * time.Second); alive(pid); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("what the addon started outlived the bot")
		}
	}
}

// alive reports whether process pid runs: it exists and isn't a zombie.
func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return true // no /proc: as signal 0 says
	}
	_, after, _ := strings.Cut(string(stat), ") ")
	return !strings.HasPrefix(after, "Z")
}

// The pseudonyms and refs the bot makes, and the context it sends, are as
// addon/schema.json, the extension's schema for addons in any language,
// says.
func TestPseudonymsMatchSchema(t *testing.T) {
	data, err := os.ReadFile("../../../addon/schema.json")
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Defs struct {
			Context struct {
				Properties map[string]struct {
					Pattern string `json:"pattern"`
				} `json:"properties"`
			} `json:"Context"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	a := newAddon("game", "Game", addonConfig{}, bytes.Repeat([]byte{7}, secretSize), nil, nil)
	g := newAddon("game", "Game", addonConfig{GlobalIDs: true}, bytes.Repeat([]byte{7}, secretSize), nil, nil)
	c := a.context(in(inA, "/play"), time.Now())
	for key, ids := range map[string][]string{
		"chat":   {c.Chat, a.chatID(inB)},
		"sender": {c.Sender, g.userID(inA, "bob-id")},
		"ref":    {c.Ref},
	} {
		re, err := regexp.Compile(s.Defs.Context.Properties[key].Pattern)
		if err != nil || s.Defs.Context.Properties[key].Pattern == "" {
			t.Fatalf("the schema's %s pattern: %v", key, err)
		}
		for _, id := range ids {
			if !re.MatchString(id) {
				t.Errorf("%s %q doesn't match the schema's %s", key, id, re)
			}
		}
	}
	if !placeholder.MatchString("{" + c.Sender + "}") {
		t.Error("a sender's placeholder isn't filled in")
	}
}
