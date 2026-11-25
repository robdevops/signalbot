package bot

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/state"
)

// keyboards are the reply keyboards posts set in DMs: each DM's labels and
// the commands they stand for. They are saved in keyboards.json, under a
// hash of each chat's key, so the file holds no ID as such (a hash could
// be matched by trying every ID, as budget.json's); it is read when first
// needed, and a state directory that can't be written keeps them in
// memory.
type keyboards struct {
	mu      sync.Mutex
	doc     *state.Doc
	loaded  bool
	failing bool                         // the last save failed, which was logged
	chats   map[string]map[string]string // by hashed chat key: label to command
}

// keyboardsName is the keyboards' saved state, which no feature may open.
const keyboardsName = state.Keyboards

// maxKeyboards caps the DMs whose keyboard is kept.
const maxKeyboards = 10000

func newKeyboards(dir string) *keyboards {
	return &keyboards{doc: state.Open(state.Dir(dir), keyboardsName), chats: map[string]map[string]string{}}
}

// hashKey is a chat's key as the file names it.
func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// load reads the saved keyboards the first time. k.mu is held.
func (k *keyboards) load() {
	if k.loaded {
		return
	}
	k.loaded = true
	var saved map[string]map[string]string
	switch found, err := k.doc.Load(&saved); {
	case err != nil:
		logx.Warnf("%v, so the DMs' keyboards start empty", err)
	case found && saved != nil:
		k.chats = saved
	}
}

// command is the command a message of text stands for in the chat whose
// key is key: one of its keyboard's labels, exactly.
func (k *keyboards) command(key, text string) (string, bool) {
	if text == "" {
		return "", false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.load()
	cmd, ok := k.chats[hashKey(key)][text]
	return cmd, ok
}

// set makes keys the keyboard of the chat whose key is key, or with none
// takes it away, and saves them all; l is the chat's network's log.
func (k *keyboards) set(l logx.L, key string, keys [][]feature.Key) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.load()
	h := hashKey(key)
	if len(keys) == 0 {
		if _, ok := k.chats[h]; !ok {
			return
		}
		delete(k.chats, h)
	} else {
		if _, ok := k.chats[h]; !ok && len(k.chats) >= maxKeyboards {
			l.Warnf("a DM's keyboard isn't kept: %d already are", maxKeyboards)
			return
		}
		m := map[string]string{}
		for _, row := range keys {
			for _, key := range row {
				m[key.Label] = key.Command
			}
		}
		k.chats[h] = m
	}
	switch err := k.doc.Save(k.chats); {
	case err != nil && !k.failing:
		logx.Warnf("the DMs' keyboards not saved, kept in memory until a save works: %v", err)
		k.failing = true
	case err == nil:
		k.failing = false
	}
}
