package syncer

import (
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/store"
)

func parse(t *testing.T, raw string) *event.Event {
	t.Helper()
	var evt event.Event
	if err := json.Unmarshal([]byte(raw), &evt); err != nil {
		t.Fatal(err)
	}
	prepare(&evt, "!room:x", event.MessageEventType)
	return &evt
}

func TestMessageWithReplyFallbackAndAttachment(t *testing.T) {
	b := store.NewBatch()
	reply := parse(t, `{"type":"m.room.message","event_id":"$r","sender":"@a:x","origin_server_ts":5,"content":{
		"msgtype":"m.text","body":"> <@b:x> original\n\nmy answer",
		"format":"org.matrix.custom.html","formatted_body":"<mx-reply><blockquote>original</blockquote></mx-reply>my answer",
		"m.relates_to":{"m.in_reply_to":{"event_id":"$orig"}}}}`)
	image := parse(t, `{"type":"m.room.message","event_id":"$i","sender":"@a:x","origin_server_ts":6,"content":{
		"msgtype":"m.image","body":"cat.jpg","file":{"url":"mxc://x/enc","v":"v2","key":{},"iv":"","hashes":{}},"info":{"mimetype":"image/jpeg"}}}`)
	sticker := parse(t, `{"type":"m.sticker","event_id":"$s","sender":"@a:x","origin_server_ts":7,"content":{"body":"wave","url":"mxc://x/s","info":{"mimetype":"image/png"}}}`)
	for _, evt := range []*event.Event{reply, image, sticker} {
		if !addMessageLike(b, evt, store.StatusPlaintext) {
			t.Fatalf("%s not stored as a message", evt.ID)
		}
	}
	if m := b.Messages[0]; m.Body != "my answer" || m.ReplyTo != "$orig" {
		t.Fatalf("reply not cleaned: %+v", m)
	}
	if att := b.Messages[1].Attachment; att == nil || att.URL != "mxc://x/enc" || att.MimeType != "image/jpeg" || att.Filename != "cat.jpg" {
		t.Fatalf("encrypted attachment wrong: %+v", att)
	}
	if m := b.Messages[2]; m.MsgType != "m.sticker" || m.Attachment == nil || m.Attachment.URL != "mxc://x/s" {
		t.Fatalf("sticker wrong: %+v", m)
	}
}

func TestEditAndRedactedEvents(t *testing.T) {
	b := store.NewBatch()
	edit := parse(t, `{"type":"m.room.message","event_id":"$e","sender":"@a:x","origin_server_ts":9,"content":{
		"msgtype":"m.text","body":"* fixed","m.new_content":{"msgtype":"m.text","body":"fixed"},
		"m.relates_to":{"rel_type":"m.replace","event_id":"$orig"}}}`)
	if addMessageLike(b, edit, store.StatusOK) || len(b.Edits) != 1 || b.Edits[0].Body != "fixed" || b.Edits[0].Target != "$orig" {
		t.Fatalf("edit not recorded: %+v", b.Edits)
	}
	redacted := parse(t, `{"type":"m.room.message","event_id":"$d","sender":"@a:x","origin_server_ts":9,"content":{},
		"unsigned":{"redacted_because":{"type":"m.room.redaction","event_id":"$x","sender":"@a:x","content":{}}}}`)
	if !addMessageLike(b, redacted, store.StatusPlaintext) || !b.Messages[0].Redacted || b.Messages[0].Body != "" {
		t.Fatalf("redacted message wrong: %+v", b.Messages)
	}
	redaction := parse(t, `{"type":"m.room.redaction","event_id":"$x","sender":"@a:x","origin_server_ts":10,"content":{"redacts":"$d"}}`)
	addRedaction(b, redaction)
	if len(b.Redactions) != 1 || b.Redactions[0].Redacts != "$d" {
		t.Fatalf("v11 redaction not parsed: %+v", b.Redactions)
	}
}

func TestUndecryptableRoundTrip(t *testing.T) {
	encrypted := `{"type":"m.room.encrypted","event_id":"$enc","sender":"@a:x","origin_server_ts":11,"content":{
		"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"abc","session_id":"S1","sender_key":"K","device_id":"D",
		"m.relates_to":{"m.in_reply_to":{"event_id":"$parent"}}}}`
	b := store.NewBatch()
	evt := parse(t, encrypted)
	addUndecryptable(b, evt, evt.Content.AsEncrypted(), sourceLive)
	u := b.Undecryptable[0]
	if u.SessionID != "S1" || !u.Live || u.Placeholder == nil || u.Placeholder.ReplyTo != "$parent" {
		t.Fatalf("undecryptable wrong: %+v", u)
	}
	var restored event.Event
	if err := json.Unmarshal(u.Raw, &restored); err != nil {
		t.Fatal(err)
	}
	prepare(&restored, restored.RoomID, event.MessageEventType)
	content, ok := restored.Content.Parsed.(*event.EncryptedEventContent)
	if !ok || content.SessionID != "S1" || restored.RoomID != "!room:x" || restored.ID != "$enc" || restored.Timestamp != 11 {
		t.Fatalf("stored event doesn't round-trip: %+v", restored)
	}

	for _, rel := range []string{`{"rel_type":"m.annotation","event_id":"$p","key":"👍"}`, `{"rel_type":"m.replace","event_id":"$p"}`} {
		b = store.NewBatch()
		evt = parse(t, `{"type":"m.room.encrypted","event_id":"$x","sender":"@a:x","origin_server_ts":1,"content":{
			"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"abc","session_id":"S2","m.relates_to":`+rel+`}}`)
		addUndecryptable(b, evt, evt.Content.AsEncrypted(), sourceBackfill)
		if len(b.Undecryptable) == 1 && b.Undecryptable[0].Placeholder != nil {
			t.Fatalf("relation %s should not get a placeholder", rel)
		}
	}
	if len(b.Undecryptable) != 1 {
		t.Fatal("an undecryptable edit should still be kept for retry")
	}
}

func TestReceiptsOnlyMine(t *testing.T) {
	s := &Syncer{me: "@me:x"}
	b := store.NewBatch()
	evt := parse(t, `{"type":"m.receipt","content":{
		"$1":{"m.read":{"@me:x":{"ts":100},"@a:x":{"ts":50}}},
		"$2":{"m.read.private":{"@me:x":{"ts":200}}},
		"$3":{"m.read":{"@me:x":{"ts":300,"thread_id":"$thread"}}}}}`)
	evt.Type.Class = event.EphemeralEventType
	evt.Content.Parsed = nil
	_ = evt.Content.ParseRaw(evt.Type)
	s.addReceipts(b, "!room:x", evt.Content.Parsed.(*event.ReceiptEventContent))
	got := map[id.EventID]int64{}
	for _, r := range b.Receipts {
		got[r.EventID] = r.TS
	}
	if len(got) != 2 || got["$1"] != 100 || got["$2"] != 200 {
		t.Fatalf("unexpected receipts: %v", got)
	}
}
