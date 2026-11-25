package mcp

import (
	"cmp"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// addonConfig is a server's "addon" block: what an addon may do, and where.
type addonConfig struct {
	Chats     []string     `json:"chats"` // group IDs, or "all"
	DMs       bool         `json:"dms"`
	Post      bool         `json:"post"`
	React     bool         `json:"react"`
	Watch     bool         `json:"watch"`
	WatchMax  string       `json:"watch_max"` // how long a watch may last: 1h by default, 24h at most
	Names     bool         `json:"names"`     // the context carries the sender's display name
	GlobalIDs bool         `json:"global_ids"`
	Rate      rateConfig   `json:"rate"`
	Model     *modelConfig `json:"model"` // nil: it may not ask the bot's model

	watchMax time.Duration // WatchMax, read
}

// modelConfig lets an addon ask the bot's model, through MCP sampling: at
// most PerHour requests an hour, of at most MaxTokens each.
type modelConfig struct {
	PerHour   int `json:"per_hour"`
	MaxTokens int `json:"max_tokens"`
}

// The model permission's defaults, and the most max_tokens may be.
const (
	defaultPerHour   = 30
	defaultMaxTokens = 4000
	maxMaxTokens     = 32000
)

// rateConfig caps an addon's posts and reactions in each chat.
type rateConfig struct {
	PerMinute int `json:"per_minute"`
	Burst     int `json:"burst"`
}

// addonSettings are the keys an addon block may have, rateSettings its
// rate's and modelSettings its model's.
var (
	addonSettings = []string{"chats", "dms", "post", "react", "watch", "watch_max", "names", "global_ids", "rate", "model"}
	rateSettings  = []string{"per_minute", "burst"}
	modelSettings = []string{"per_hour", "max_tokens"}
)

// coreSettings are signalbot's own settings, docs/CONFIGURATION.md's table
// (a test keeps them in step): an addon is given the variables that start
// with its prefix, so no prefix may start one of these.
var coreSettings = []string{
	"SIGNALCLI_SOCKET", "SIGNAL_DM_USER_IDS", "TELEGRAM_BOT_TOKEN", "TELEGRAM_DM_USER_IDS", "TELEGRAM_API_URL",
	"MODEL", "MODEL_API_KEY", "MODEL_API_URL", "MODEL_REASONING", "MODEL_SEARCH", "MODEL_SYSTEM_PROMPT",
	"CHARTS", "IMAGE_MODEL", "IMAGE_QUALITY", "DAILY_BUDGET", "DARKMODE", "FILES", "GROUP_CONTEXT", "HISTORY_MAX",
	"INTRO", "MAX_CONCURRENCY", "MCP_CONFIG", "STATE_DIRECTORY", "TOOL_LINGER_ROUNDS", "TZ",
}

// envPrefix is what an addon server's settings start with: its name in
// capitals, anything but letters and digits as _, then _.
func envPrefix(server string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		}
		return '_'
	}, server) + "_"
}

// telegramGroup is a Telegram group's ID, as the bot names it.
var telegramGroup = regexp.MustCompile(`^-[0-9]{1,20}$`)

// isGroupID reports whether id is a group's ID: Signal's, 32 bytes in
// base64, or Telegram's, a negative number.
func isGroupID(id string) bool {
	b, err := base64.StdEncoding.DecodeString(id)
	return err == nil && len(b) == 32 || telegramGroup.MatchString(id)
}

// check checks an addon block, whose raw keys are entry, and fills in its
// defaults.
func (a *addonConfig) check(entry map[string]json.RawMessage) error {
	if err := unknown(entry, addonSettings); err != nil {
		return fmt.Errorf("addon: %v", err)
	}
	var rate map[string]json.RawMessage
	json.Unmarshal(entry["rate"], &rate)
	if err := unknown(rate, rateSettings); err != nil {
		return fmt.Errorf("addon rate: %v", err)
	}
	var model map[string]json.RawMessage
	json.Unmarshal(entry["model"], &model)
	if err := unknown(model, modelSettings); err != nil {
		return fmt.Errorf("addon model: %v", err)
	}
	for _, c := range a.Chats {
		switch {
		case c == "all" && len(a.Chats) > 1:
			return errors.New(`addon chats: "all" goes alone`)
		case c != "all" && !isGroupID(c):
			return fmt.Errorf(`addon chats: %q isn't a group ID; copy the group's ID as Signal or Telegram shows it, or use "all"`, txt.Clamp(c, 60))
		}
	}
	a.watchMax = time.Hour
	if a.WatchMax != "" {
		d, err := time.ParseDuration(a.WatchMax)
		if err != nil || d <= 0 || d > 24*time.Hour {
			return fmt.Errorf("addon watch_max: %q isn't a time from 1s to 24h, such as 1h or 30m", txt.Clamp(a.WatchMax, 40))
		}
		a.watchMax = d
	}
	if a.Rate.PerMinute < 0 || a.Rate.Burst < 0 {
		return errors.New("addon rate: per_minute and burst can't be below 0")
	}
	a.Rate.PerMinute, a.Rate.Burst = cmp.Or(a.Rate.PerMinute, 20), cmp.Or(a.Rate.Burst, 5)
	if m := a.Model; m != nil {
		switch {
		case m.PerHour < 0 || m.MaxTokens < 0:
			return errors.New("addon model: per_hour and max_tokens can't be below 0")
		case m.MaxTokens > maxMaxTokens:
			return fmt.Errorf("addon model: max_tokens can't be over %d", maxMaxTokens)
		}
		m.PerHour, m.MaxTokens = cmp.Or(m.PerHour, defaultPerHour), cmp.Or(m.MaxTokens, defaultMaxTokens)
	}
	return nil
}

// checkAddonServer checks what an addon server may not have: a url, an env
// block, or a prefix one of the bot's own settings starts with.
func checkAddonServer(name string, s server) error {
	prefix := envPrefix(name)
	switch {
	case s.Command == "":
		return errors.New("an addon needs a command: addons run only as local servers")
	case s.Env != nil:
		return fmt.Errorf("an addon server takes no env block: put its settings in the bot's environment, starting with %s, and it gets them", prefix)
	}
	if i := slices.IndexFunc(coreSettings, func(s string) bool { return strings.HasPrefix(s, prefix) }); i >= 0 {
		return fmt.Errorf("an addon's settings start with %s, as the bot's own %s does; rename the server", prefix, coreSettings[i])
	}
	return nil
}

// shared are the bot's variables every addon gets, if set: the time zone,
// and how to reach the internet (a proxy, and certificates to trust). None
// holds a key of the bot's.
var shared = []string{"TZ", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "NO_PROXY", "no_proxy", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// addonEnv is an addon's whole environment: PATH, HOME, the shared
// variables that are set, and the bot's variables that start with prefix.
// stateDir, if not "", is the addon's <prefix>STATE_DIR unless the bot's
// environment sets one that isn't blank; the addon makes the directory
// itself.
func addonEnv(prefix, stateDir string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for _, name := range shared {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	// A blank <prefix>STATE_DIR counts as unset, as the bot's own settings do.
	own := strings.TrimSpace(os.Getenv(prefix+"STATE_DIR")) != ""
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, prefix) {
			continue
		}
		if !own && stateDir != "" && strings.HasPrefix(kv, prefix+"STATE_DIR=") {
			continue
		}
		env = append(env, kv)
	}
	if !own && stateDir != "" {
		env = append(env, prefix+"STATE_DIR="+stateDir)
	}
	return env
}

// safeDirName is a server name that can be a directory's name as it is.
var safeDirName = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,63}$`)

// addonStateDir is where an addon server called name keeps its own files by
// default: addons/<name> in the bot's state directory, the first in
// stateDirectory (STATE_DIRECTORY's value), made absolute. It is "" when
// STATE_DIRECTORY isn't set, or the name isn't letters, digits, ., _ and -
// (up to 64, not starting with .), so no name can reach outside addons.
func addonStateDir(stateDirectory, name string) string {
	dir, _, _ := strings.Cut(stateDirectory, ":")
	if dir == "" || !safeDirName.MatchString(name) {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, "addons", name)
}

// saved is the mcp store's document.
type saved struct {
	Secret []byte                `json:"secret"`           // the key every pseudonym is made from
	Addons map[string]addonState `json:"addons,omitempty"` // each addon's state, by server name
}

// secretSize is the pseudonym secret's size, in bytes.
const secretSize = 32

// loadSaved is the mcp store's document, with the pseudonym secret made and
// saved the first time. One that can't be read or saved is an error: never
// a new secret, which would change every pseudonym.
func loadSaved(store feature.Store) (saved, error) {
	var s saved
	found, err := store.Load(&s)
	switch {
	case err != nil:
		return saved{}, err
	case found && len(s.Secret) == secretSize:
		return s, nil
	case found:
		return saved{}, errors.New("the saved secret is damaged")
	}
	s.Secret = make([]byte, secretSize)
	rand.Read(s.Secret)
	if err := store.Save(s); err != nil {
		return saved{}, err
	}
	return s, nil
}

// encoding writes pseudonyms and refs: lowercase base32, no padding.
var encoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// placeholder is a {u_…} an addon writes for a person's name.
var placeholder = regexp.MustCompile(`\{u_[a-z2-7]{26}\}`)

// Kept for: a ref, and a sender's last-seen name.
const (
	refLife  = 24 * time.Hour
	nameLife = 24 * time.Hour
)

// addon is a server that speaks signalbot's extension, and what the bot
// keeps for it. Its watches, claims, leases and outbox cursor are saved,
// by the keeper; the rest is in memory only.
type addon struct {
	server   string // its name in MCP_CONFIG
	title    string // its name for people, as it gave it
	cfg      addonConfig
	key      []byte          // HMAC(secret, server): its pseudonyms' key
	conn     conn            // where to call it
	bot      map[string]bool // the tools the bot may call itself
	networks bool            // the bot is on several networks: each context names its chat's
	busy     chan struct{}   // a place for each direct call in flight
	now      func() time.Time
	host     feature.Host  // where it posts and reacts; set before it runs
	changed  func()        // its saved state changed: save it soon, without waiting
	dirty    chan struct{} // its outbox has news: read it
	up       chan struct{} // it has started again
	settled  chan struct{} // its posts read from the outbox have all been sent or dropped
	matches  chan match    // matched messages, for the watch worker
	posts    *limiter      // its posts and reactions, per chat
	calls    *limiter      // the watch worker's calls, per chat
	asks     *limiter      // its requests to the model, all chats as one; nil without the permission

	mu      sync.Mutex
	refs    map[string]ref         // by ref
	names   map[string]seen        // by chat key, "\x00", sender's pseudonym
	watches map[string][]*watch    // live, by chat key
	claims  map[claimKey]claim     // messages its watches took
	leases  map[string]lease       // the chats it may post in, by pseudonym
	tombs   map[string]time.Time   // unwatches before their watch, by chat pseudonym, "\x00", watch ID: until when
	over    map[string][]ended     // finished watches' set answers, by chat key; in memory only
	cursor  string                 // where its outbox was read and delivered up to; "" for the start
	next    string                 // where the last read of its outbox ended: the cursor once its posts are sent
	missed  bool                   // a post read from the outbox wasn't sent: read it again before the cursor moves
	applied []string               // the IDs of its last ops applied, oldest first
	done    map[string]bool        // applied, as a set
	sending map[string]bool        // the IDs of post ops held or in a mailbox
	failed  map[string]failure     // post ops dropped before they were sent, by ID
	waiting map[string][]*outgoing // by chat pseudonym, while a post of its waits in the chat's mailbox: those held to send after it, in turn
	posted  map[string]posted      // the post ops it sent, by ID, for a later post's replyTo
}

// ref is the message a ref stands for: who sent it, where and when.
type ref struct {
	msg     feature.Message // without its text
	expires time.Time
}

// seen is the name a sender last had in a chat.
type seen struct {
	name    string
	expires time.Time
}

func newAddon(server, title string, cfg addonConfig, secret []byte, c conn, bot map[string]bool) *addon {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(server))
	return &addon{server: server, title: title, cfg: cfg, key: mac.Sum(nil), conn: c, bot: bot,
		busy: make(chan struct{}, maxDirect), now: time.Now, changed: func() {}, dirty: make(chan struct{}, 1),
		up: make(chan struct{}, 1), settled: make(chan struct{}, 1),
		matches: make(chan match, maxMatches), posts: newLimiter(cfg.Rate), calls: newLimiter(watchRate), asks: hourly(cfg.Model),
		refs: map[string]ref{}, names: map[string]seen{}, watches: map[string][]*watch{}, claims: map[claimKey]claim{},
		leases: map[string]lease{}, tombs: map[string]time.Time{}, done: map[string]bool{},
		sending: map[string]bool{}, failed: map[string]failure{}, waiting: map[string][]*outgoing{}, posted: map[string]posted{}}
}

// allows reports whether the addon may be used in chat.
func (a *addon) allows(chat feature.Chat) bool {
	if chat.GroupID == "" {
		return a.cfg.DMs
	}
	group, _, _ := strings.Cut(chat.GroupID, "/") // a Telegram topic is in its group
	return slices.Contains(a.cfg.Chats, "all") || slices.Contains(a.cfg.Chats, chat.GroupID) || slices.Contains(a.cfg.Chats, group)
}

// pseudonym is prefix and HMAC(key, data), cut to 128 bits.
func (a *addon) pseudonym(prefix, data string) string {
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(data))
	return prefix + encoding.EncodeToString(mac.Sum(nil)[:16])
}

// chatID is the addon's name for chat.
func (a *addon) chatID(chat feature.Chat) string { return a.pseudonym("c_", "chat\x00"+chat.Key()) }

// userID is the addon's name for sender: in chat only, unless global_ids.
func (a *addon) userID(chat feature.Chat, sender string) string {
	if a.cfg.GlobalIDs {
		return a.pseudonym("u_", "user\x00"+sender)
	}
	return a.pseudonym("u_", "user\x00"+chat.Key()+"\x00"+sender)
}

// context is m's context for the addon, which the bot got at at. It keeps
// the sender's name, for placeholders, and a ref for m. sent and edited are
// when the bot got it, never the sender's clock.
func (a *addon) context(m feature.Message, at time.Time) ext.Context {
	now := a.now()
	user := a.userID(m.Chat, m.Sender)
	r := make([]byte, 16)
	rand.Read(r)
	id := "r_" + encoding.EncodeToString(r)
	a.mu.Lock()
	for k, v := range a.refs {
		if now.After(v.expires) {
			delete(a.refs, k)
		}
	}
	for k, v := range a.names {
		if now.After(v.expires) {
			delete(a.names, k)
		}
	}
	a.refs[id] = ref{feature.Message{Chat: m.Chat, Sender: m.Sender, Timestamp: m.Timestamp, Edited: m.Edited}, now.Add(refLife)}
	if m.Name != "" {
		a.names[m.Chat.Key()+"\x00"+user] = seen{m.Name, now.Add(nameLife)}
	}
	a.mu.Unlock()
	c := ext.Context{Chat: a.chatID(m.Chat), Sender: user, Ref: id, Addressed: m.Addressed, Group: m.Chat.GroupID != "", Sent: at.Unix()}
	if a.networks {
		c.Network = a.host.Net(m.Chat).Name
	}
	if m.Edited != 0 {
		c.Edited = c.Sent
	}
	if a.cfg.Names {
		c.Name = m.Name
	}
	return c
}

// fill replaces each {u_…} in text with @Name, the name the addon last saw
// that sender use in chat, or "someone".
func (a *addon) fill(chat feature.Chat, text string) string {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	return placeholder.ReplaceAllStringFunc(text, func(p string) string {
		if s, ok := a.names[chat.Key()+"\x00"+p[1:len(p)-1]]; ok && now.Before(s.expires) {
			return "@" + s.name
		}
		return "someone"
	})
}
