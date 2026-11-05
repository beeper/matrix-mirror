package cli

import (
	"encoding/json"
	"errors"
	"io"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/store"
)

// JSON output is the project's public contract: within a version, shapes only
// change additively.
const jsonVersion = 1

type jsonChat struct {
	RoomID      id.RoomID `json:"room_id"`
	Name        string    `json:"name"`
	Network     *string   `json:"network"`
	NetworkName *string   `json:"network_name"`
	IsDM        bool      `json:"is_dm"`
	Unread      int       `json:"unread"`
	LastEventTS *int64    `json:"last_event_ts"`
	LastEventAt *string   `json:"last_event_at"`
	MyReadTS    *int64    `json:"my_read_ts"`
}

type jsonReply struct {
	EventID    id.EventID `json:"event_id"`
	Sender     *id.UserID `json:"sender"`
	SenderName *string    `json:"sender_name"`
	Body       *string    `json:"body"`
}

type jsonMessage struct {
	EventID       id.EventID        `json:"event_id"`
	RoomID        id.RoomID         `json:"room_id"`
	Sender        id.UserID         `json:"sender"`
	SenderName    *string           `json:"sender_name"`
	FromMe        bool              `json:"from_me"`
	TS            int64             `json:"ts"`
	Time          string            `json:"time"`
	MsgType       *string           `json:"msgtype"`
	Body          *string           `json:"body"`
	ReplyTo       *jsonReply        `json:"reply_to"`
	EditedAt      *int64            `json:"edited_at"`
	Redacted      bool              `json:"redacted"`
	DecryptStatus string            `json:"decrypt_status"`
	Attachment    *store.Attachment `json:"attachment"`
}

type jsonSearchResult struct {
	jsonMessage
	ChatName string `json:"chat_name"`
	Snippet  string `json:"snippet"`
}

type jsonUnreadChat struct {
	jsonChat
	Latest *jsonMessage `json:"latest"`
}

type jsonError struct {
	Code       string     `json:"code"`
	Message    string     `json:"message"`
	Candidates []jsonChat `json:"candidates,omitempty"`
}

func writeJSON(w io.Writer, payload map[string]any) error {
	payload["v"] = jsonVersion
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func writeJSONError(w io.Writer, err error) {
	jerr := jsonError{Code: "error", Message: err.Error()}
	var ambiguous *store.AmbiguousChatError
	switch {
	case errors.As(err, &ambiguous):
		jerr.Code = "ambiguous_chat"
		for _, c := range ambiguous.Candidates {
			jerr.Candidates = append(jerr.Candidates, toJSONChat(c))
		}
	case errors.Is(err, store.ErrChatNotFound):
		jerr.Code = "chat_not_found"
	case errors.Is(err, store.ErrNoDatabase):
		jerr.Code = "no_database"
	case errors.Is(err, errInvalidArgument), errors.Is(err, store.ErrEmptyQuery):
		jerr.Code = "invalid_argument"
	}
	_ = writeJSON(w, map[string]any{"error": jerr})
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func optTS(ts int64) *int64 {
	if ts == 0 {
		return nil
	}
	return &ts
}

func isoTime(ts int64) string {
	return time.UnixMilli(ts).Local().Format(time.RFC3339)
}

func toJSONChat(c store.Chat) jsonChat {
	out := jsonChat{
		RoomID:      c.RoomID,
		Name:        c.Name,
		Network:     optString(c.Network),
		NetworkName: optString(c.NetworkName),
		IsDM:        c.IsDM,
		Unread:      c.Unread,
		LastEventTS: optTS(c.LastEventTS),
		MyReadTS:    optTS(c.MyReadTS),
	}
	if c.LastEventTS != 0 {
		at := isoTime(c.LastEventTS)
		out.LastEventAt = &at
	}
	return out
}

func toJSONMessage(m store.MessageRow, me id.UserID) jsonMessage {
	out := jsonMessage{
		EventID:       m.EventID,
		RoomID:        m.RoomID,
		Sender:        m.Sender,
		SenderName:    optString(m.SenderName),
		FromMe:        m.Sender == me,
		TS:            m.TS,
		Time:          isoTime(m.TS),
		MsgType:       optString(m.MsgType),
		Body:          m.Body,
		EditedAt:      m.EditedAt,
		Redacted:      m.Redacted,
		DecryptStatus: m.DecryptStatus,
		Attachment:    m.Attachment,
	}
	if m.ReplyTo != nil {
		out.ReplyTo = &jsonReply{EventID: m.ReplyTo.EventID, SenderName: optString(m.ReplyTo.SenderName), Body: m.ReplyTo.Body}
		if m.ReplyTo.Sender != "" {
			sender := m.ReplyTo.Sender
			out.ReplyTo.Sender = &sender
		}
	}
	return out
}
