package signal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

// Send posts msg; mentions are signal-cli specs ("start:length:uuid") for @names in msg.
// GetAttachment fetches an attachment's bytes from signal-cli by id, naming
// the chat it arrived in: the group, or for a DM the person who sent it. The
// bot never reads signal-cli's data directory itself.
func (s *Client) GetAttachment(ctx context.Context, t Target, id string) ([]byte, error) {
	p := map[string]any{"id": id}
	if t.GroupID != "" {
		p["groupId"] = t.GroupID
	} else {
		p["recipient"] = t.User
	}
	resp, err := s.call(ctx, "getAttachment", p)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, errors.New("malformed getAttachment result")
	}
	return base64.StdEncoding.DecodeString(r.Data)
}

// GetSticker fetches a sticker's image (WebP, or APNG if animated) from
// signal-cli, which downloads the packs of stickers it receives.
func (s *Client) GetSticker(ctx context.Context, st Sticker) ([]byte, error) {
	resp, err := s.call(ctx, "getSticker", map[string]any{"packId": st.PackID, "stickerId": st.StickerID})
	if err != nil {
		return nil, err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, errors.New("malformed getSticker result")
	}
	return base64.StdEncoding.DecodeString(r.Data)
}

// StickerLabel describes a sticker from its pack's details: its emoji and
// the pack's title, e.g. `: 🕵️ from "My Daily Life"`, or "" if signal-cli
// doesn't know the pack. Both come from whoever made the pack, so they are
// trimmed and the title quoted.
func (s *Client) StickerLabel(ctx context.Context, st Sticker) string {
	resp, err := s.call(ctx, "listStickerPacks", nil)
	if err != nil {
		return ""
	}
	var packs []struct {
		PackID   string `json:"packId"`
		Title    string `json:"title"`
		Stickers []struct {
			ID    int    `json:"id"`
			Emoji string `json:"emoji"`
		} `json:"stickers"`
	}
	if json.Unmarshal(resp.Result, &packs) != nil {
		return ""
	}
	for _, p := range packs {
		if p.PackID != st.PackID {
			continue
		}
		label := ""
		for _, sk := range p.Stickers {
			if sk.ID == st.StickerID {
				if e := txt.Clamp(txt.Safe(sk.Emoji), 8); e != "" {
					label = ": " + e
				}
			}
		}
		if t := txt.Safe(p.Title); t != "" {
			label += " from " + strconv.Quote(t)
		}
		return label
	}
	return ""
}
