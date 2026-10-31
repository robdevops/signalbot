package signal

import ()

type Mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

type Quote struct {
	Author      string       `json:"author"`
	AuthorUUID  string       `json:"authorUuid"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment is a file on a message. A quote's attachments carry only a
// small thumbnail of the original.
type Attachment struct {
	ContentType string      `json:"contentType"`
	ID          string      `json:"id"`
	Size        int         `json:"size"`
	Thumbnail   *Attachment `json:"thumbnail"`
}

// Sticker names a sticker by its pack and its index in the pack.
type Sticker struct {
	PackID    string `json:"packId"`
	StickerID int    `json:"stickerId"`
}

// Image is the attachment itself if it is an image the model can be shown,
// else its thumbnail if that is, else nil.
func (a Attachment) Image() *Attachment {
	for _, c := range []*Attachment{&a, a.Thumbnail} {
		if c == nil || c.ID == "" || c.Size > MaxImageBytes {
			continue
		}
		switch c.ContentType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return c
		}
	}
	return nil
}

type GroupInfo struct {
	GroupID string `json:"groupId"`
	Type    string `json:"type"` // "UPDATE" when the group's members, name or description changed
}

type DataMessage struct {
	Message     string       `json:"message"`
	Mentions    []Mention    `json:"mentions"`
	Quote       *Quote       `json:"quote"`
	GroupInfo   *GroupInfo   `json:"groupInfo"`
	Attachments []Attachment `json:"attachments"`
	Sticker     *Sticker     `json:"sticker"`

	// ExpiresInSeconds is the chat's disappearing-messages timer, 0 if off.
	ExpiresInSeconds int `json:"expiresInSeconds"`
}

type Envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	DataMessage  *DataMessage `json:"dataMessage"`
}

// MaxImageBytes caps one image, sent or received.
const MaxImageBytes = 5 << 20

// Target addresses a chat: a group or a single recipient.
type Target struct {
	GroupID string
	User    string
}

func (t Target) params() map[string]any {
	if t.GroupID != "" {
		return map[string]any{"groupId": t.GroupID}
	}
	return map[string]any{"recipient": []string{t.User}}
}
