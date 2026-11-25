package slack

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/fsx"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// ManifestFile is the app manifest's file name in the state directory.
const ManifestFile = "slack-manifest.json"

// manifest is a Slack app manifest: the app's name, its bot user, its
// slash commands, the scopes and events the bot needs, Socket Mode and
// buttons. Slack's slash commands live only here, which a bot token can't
// change, so the bot writes it for its owner to paste into the app's
// settings.
type manifest struct {
	DisplayInformation struct {
		Name string `json:"name"`
	} `json:"display_information"`
	Features struct {
		BotUser struct {
			DisplayName  string `json:"display_name"`
			AlwaysOnline bool   `json:"always_online"`
		} `json:"bot_user"`
		SlashCommands []slashCommand `json:"slash_commands,omitempty"`
	} `json:"features"`
	OAuthConfig struct {
		Scopes struct {
			Bot []string `json:"bot"`
		} `json:"scopes"`
	} `json:"oauth_config"`
	Settings struct {
		EventSubscriptions struct {
			BotEvents []string `json:"bot_events"`
		} `json:"event_subscriptions"`
		Interactivity struct {
			IsEnabled bool `json:"is_enabled"`
		} `json:"interactivity"`
		SocketModeEnabled bool `json:"socket_mode_enabled"`
		OrgDeployEnabled  bool `json:"org_deploy_enabled"`
	} `json:"settings"`
}

type slashCommand struct {
	Command      string `json:"command"`
	Description  string `json:"description"`
	ShouldEscape bool   `json:"should_escape"`
}

// slashName is a name Slack takes for a slash command.
var slashName = regexp.MustCompile(`^[a-z0-9_-]{1,31}$`)

// Manifest is the app manifest for a bot named name with cmds as its
// slash commands, each described by what it is about or its own name; a
// name Slack doesn't take is left out.
func Manifest(name string, cmds []bot.Command) []byte {
	var m manifest
	name = cmp.Or(name, "signalbot")
	m.DisplayInformation.Name = txt.Clamp(name, 35)
	m.Features.BotUser.DisplayName = txt.Clamp(name, 80)
	m.Features.BotUser.AlwaysOnline = true
	for _, c := range cmds {
		if slashName.MatchString(c.Name) && len(m.Features.SlashCommands) < 50 {
			m.Features.SlashCommands = append(m.Features.SlashCommands, slashCommand{Command: "/" + c.Name, Description: txt.Clamp(cmp.Or(c.About, c.Name), 2000)})
		}
	}
	m.OAuthConfig.Scopes.Bot = []string{"app_mentions:read", "channels:history", "channels:read", "groups:history", "groups:read", "im:history", "im:write", "chat:write",
		"commands", "files:read", "files:write", "reactions:read", "reactions:write", "users:read"}
	m.Settings.EventSubscriptions.BotEvents = []string{"app_mention", "message.channels", "message.groups", "message.im",
		"reaction_added", "reaction_removed", "member_joined_channel", "member_left_channel"}
	m.Settings.Interactivity.IsEnabled = true
	m.Settings.SocketModeEnabled = true
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n')
}

// writeManifest writes the app manifest, with the commands features
// registered, into the state directory if it differs from the one there,
// and says to paste it into the app's settings. Until users.info has told
// the bot its display name, it keeps the name in the manifest already
// there, or writes none, so a failed lookup doesn't rename the app.
func (c *Client) writeManifest() {
	cmds := c.commands.Load()
	if cmds == nil {
		return
	}
	path := filepath.Join(c.stateDir, ManifestFile)
	old, err := os.ReadFile(path)
	name := c.BotName()
	if !c.named.Load() {
		var m manifest
		if err != nil || json.Unmarshal(old, &m) != nil || m.Features.BotUser.DisplayName == "" {
			logx.Debugf("slack: the app manifest waits for the bot's display name")
			return
		}
		name = m.Features.BotUser.DisplayName
	}
	data := Manifest(name, *cmds)
	if err == nil && bytes.Equal(old, data) {
		return
	}
	if err := fsx.WriteAtomic(path, data); err != nil {
		logx.Warnf("slack: writing the app manifest failed: %v", err)
		return
	}
	logx.Noticef("Slack: the commands changed: paste %s at api.slack.com/apps → your app → App Manifest", path)
}

// manifestDirs are where SavedManifest looks when STATE_DIRECTORY isn't
// set, in turn: the systemd unit's state directory, the Docker image's,
// and the working directory. Tests change it.
var manifestDirs = []string{"/var/lib/private/signalbot", "/data", ""}

// SavedManifest is the app manifest the bot last wrote, and the file it
// came from. It looks in STATE_DIRECTORY's directory alone when that is
// set, and otherwise in manifestDirs. If it finds none it is one with no
// slash commands, "", and the files it tried, each with why it couldn't
// read one that is there.
func SavedManifest() (data []byte, path string, tried []string) {
	dirs := manifestDirs
	if dir := config.StateDir(); dir != "" {
		dirs = []string{dir}
	}
	for _, dir := range dirs {
		p := filepath.Join(dir, ManifestFile)
		data, err := os.ReadFile(p)
		switch {
		case err == nil:
			return data, p, nil
		case errors.Is(err, fs.ErrNotExist):
			tried = append(tried, p)
		case errors.Is(err, fs.ErrPermission):
			tried = append(tried, p+" (permission denied: try sudo)")
		default:
			tried = append(tried, p+" (unreadable)")
		}
	}
	return Manifest("", nil), "", tried
}
