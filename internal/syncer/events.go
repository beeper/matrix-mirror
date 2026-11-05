package syncer

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/store"
)

type source int

const (
	sourceLive source = iota
	sourceBackfill
	sourceRetry
)

// prepare sets the room and type class that /sync and /messages leave out, then
// parses the content. Unknown event types are left unparsed.
func prepare(evt *event.Event, roomID id.RoomID, class event.TypeClass) {
	if roomID != "" {
		evt.RoomID = roomID
	}
	if evt.StateKey != nil {
		class = event.StateEventType
	}
	evt.Type.Class = class
	if evt.Content.Parsed == nil {
		_ = evt.Content.ParseRaw(evt.Type)
	}
}

func isRedacted(evt *event.Event) bool {
	return evt.Unsigned.RedactedBecause != nil
}

func (s *Syncer) applyState(ctx context.Context, b *store.Batch, ru *store.RoomUpdate, evt *event.Event) {
	switch content := evt.Content.Parsed.(type) {
	case *event.RoomNameEventContent:
		ru.ExplicitName = &content.Name
	case *event.CanonicalAliasEventContent:
		alias := content.Alias.String()
		ru.CanonicalAlias = &alias
	case *event.CreateEventContent:
		roomType := string(content.Type)
		ru.RoomType = &roomType
	case *event.BridgeEventContent:
		network, networkName, bot := content.Protocol.ID, content.Protocol.DisplayName, content.BridgeBot.String()
		ru.Network, ru.NetworkName, ru.BridgeBot = &network, &networkName, &bot
		if content.BeeperRoomTypeV2 == "dm" || (content.BeeperRoomTypeV2 == "" && content.BeeperRoomType == "dm") {
			ru.IsDM = true
		}
	case *event.EncryptionEventContent:
		ru.Encrypted = true
		if err := s.crypto.StateStore.SetEncryptionEvent(ctx, evt.RoomID, content); err != nil {
			s.log.Warn().Err(err).Stringer("room_id", evt.RoomID).Msg("Failed to store encryption event")
		}
	case *event.MemberEventContent:
		userID := id.UserID(evt.GetStateKey())
		b.Members = append(b.Members, memberFrom(evt.RoomID, userID, content))
		if userID == s.me && content.IsDirect {
			ru.IsDM = true
		}
	}
}

func memberFrom(roomID id.RoomID, userID id.UserID, content *event.MemberEventContent) store.Member {
	return store.Member{RoomID: roomID, UserID: userID, DisplayName: content.Displayname, Membership: string(content.Membership)}
}

func (s *Syncer) handleEvent(ctx context.Context, b *store.Batch, evt *event.Event, src source) {
	switch evt.Type {
	case event.EventEncrypted:
		s.handleEncrypted(ctx, b, evt, src)
	case event.EventMessage, event.EventSticker:
		addMessageLike(b, evt, store.StatusPlaintext)
	case event.EventRedaction:
		addRedaction(b, evt)
	}
}

func (s *Syncer) handleEncrypted(ctx context.Context, b *store.Batch, evt *event.Event, src source) {
	content, ok := evt.Content.Parsed.(*event.EncryptedEventContent)
	if !ok || isRedacted(evt) {
		return
	}
	decrypted, err := s.mach.DecryptMegolmEvent(ctx, evt)
	if err != nil {
		zerolog.Ctx(ctx).Debug().Err(err).
			Stringer("event_id", evt.ID).
			Stringer("session_id", content.SessionID).
			Msg("Couldn't decrypt event yet")
		addUndecryptable(b, evt, content, src)
		return
	}
	b.Resolved = append(b.Resolved, evt.ID)
	prepare(decrypted, evt.RoomID, event.MessageEventType)
	isMessage := false
	switch decrypted.Type {
	case event.EventMessage, event.EventSticker:
		isMessage = addMessageLike(b, decrypted, store.StatusOK)
	case event.EventRedaction:
		addRedaction(b, decrypted)
	}
	if !isMessage {
		b.Discard = append(b.Discard, evt.ID)
	}
}

// addMessageLike records a message, or an edit if it replaces another event.
// It reports whether a message row was produced.
func addMessageLike(b *store.Batch, evt *event.Event, status string) bool {
	content, ok := evt.Content.Parsed.(*event.MessageEventContent)
	if !ok {
		return false
	}
	if target := content.RelatesTo.GetReplaceID(); target != "" {
		newContent := content.NewContent
		if newContent == nil {
			newContent = content
		}
		b.Edits = append(b.Edits, store.Edit{
			EventID:    evt.ID,
			Target:     target,
			Sender:     evt.Sender,
			TS:         evt.Timestamp,
			MsgType:    msgType(evt, newContent),
			Body:       newContent.Body,
			Attachment: attachment(evt, newContent),
		})
		return false
	}
	msg := store.Message{
		EventID:       evt.ID,
		RoomID:        evt.RoomID,
		Sender:        evt.Sender,
		TS:            evt.Timestamp,
		DecryptStatus: status,
	}
	if isRedacted(evt) {
		msg.Redacted = true
	} else {
		content.RemoveReplyFallback()
		msg.MsgType = msgType(evt, content)
		msg.Body = content.Body
		msg.ReplyTo = content.RelatesTo.GetReplyTo()
		msg.Attachment = attachment(evt, content)
	}
	b.Messages = append(b.Messages, msg)
	return true
}

func msgType(evt *event.Event, content *event.MessageEventContent) string {
	if evt.Type == event.EventSticker {
		return "m.sticker"
	}
	return string(content.MsgType)
}

func attachment(evt *event.Event, content *event.MessageEventContent) *store.Attachment {
	if evt.Type != event.EventSticker && !content.MsgType.IsMedia() {
		return nil
	}
	url := content.URL
	if content.File != nil {
		url = content.File.URL
	}
	att := &store.Attachment{Filename: content.GetFileName(), URL: string(url)}
	if content.Info != nil {
		att.MimeType = content.Info.MimeType
	}
	if att.URL == "" && att.Filename == "" {
		return nil
	}
	return att
}

func addRedaction(b *store.Batch, evt *event.Event) {
	redacts := evt.Redacts
	if content, ok := evt.Content.Parsed.(*event.RedactionEventContent); ok && content.Redacts != "" {
		redacts = content.Redacts
	}
	if redacts != "" {
		b.Redactions = append(b.Redactions, store.Redaction{EventID: evt.ID, Redacts: redacts, TS: evt.Timestamp})
	}
}

// storedEvent is the subset of an encrypted event needed to retry decrypting it.
type storedEvent struct {
	Type      string          `json:"type"`
	EventID   id.EventID      `json:"event_id"`
	RoomID    id.RoomID       `json:"room_id"`
	Sender    id.UserID       `json:"sender"`
	Timestamp int64           `json:"origin_server_ts"`
	Content   json.RawMessage `json:"content"`
}

func addUndecryptable(b *store.Batch, evt *event.Event, content *event.EncryptedEventContent, src source) {
	rel := content.RelatesTo
	if rel != nil && rel.Type == event.RelAnnotation {
		return
	}
	raw, err := json.Marshal(storedEvent{
		Type:      evt.Type.Type,
		EventID:   evt.ID,
		RoomID:    evt.RoomID,
		Sender:    evt.Sender,
		Timestamp: evt.Timestamp,
		Content:   evt.Content.VeryRaw,
	})
	if err != nil {
		return
	}
	u := store.Undecryptable{
		EventID:   evt.ID,
		RoomID:    evt.RoomID,
		Sender:    evt.Sender,
		SessionID: content.SessionID,
		SenderKey: content.SenderKey,
		DeviceID:  content.DeviceID,
		Raw:       raw,
		Live:      src == sourceLive,
	}
	if rel.GetReplaceID() == "" {
		u.Placeholder = &store.Message{
			EventID:       evt.ID,
			RoomID:        evt.RoomID,
			Sender:        evt.Sender,
			TS:            evt.Timestamp,
			ReplyTo:       rel.GetReplyTo(),
			DecryptStatus: store.StatusUndecryptable,
		}
	}
	b.Undecryptable = append(b.Undecryptable, u)
}

func (s *Syncer) addReceipts(b *store.Batch, roomID id.RoomID, content *event.ReceiptEventContent) {
	for eventID, receipts := range *content {
		for _, receiptType := range []event.ReceiptType{event.ReceiptTypeRead, event.ReceiptTypeReadPrivate} {
			receipt, ok := receipts[receiptType][s.me]
			if !ok || (receipt.ThreadID != "" && receipt.ThreadID != event.ReadReceiptThreadMain) {
				continue
			}
			var ts int64
			if !receipt.Timestamp.IsZero() {
				ts = receipt.Timestamp.UnixMilli()
			}
			b.Receipts = append(b.Receipts, store.Receipt{RoomID: roomID, EventID: eventID, TS: ts})
		}
	}
}
