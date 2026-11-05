package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"maunium.net/go/mautrix/id"
)

const (
	me    = id.UserID("@me:example.org")
	alice = id.UserID("@alice:example.org")
	bot   = id.UserID("@whatsappbot:example.org")
	room  = id.RoomID("!room:example.org")
)

func setup(t *testing.T) (*Writer, *Reader, context.Context) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mirror.db")
	w, err := OpenWriter(ctx, path, me)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = w.Close() })
	r, err := OpenReader(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return w, r, ctx
}

func apply(t *testing.T, w *Writer, ctx context.Context, fill func(b *Batch)) {
	t.Helper()
	b := NewBatch()
	fill(b)
	if err := w.Apply(ctx, b); err != nil {
		t.Fatal(err)
	}
}

func joinRoom(b *Batch, prevBatch string) {
	ru := b.Room(room)
	ru.Membership = MembershipJoin
	ru.PrevBatch = prevBatch
}

func text(eventID id.EventID, sender id.UserID, ts int64, body string) Message {
	return Message{EventID: eventID, RoomID: room, Sender: sender, TS: ts, MsgType: "m.text", Body: body, DecryptStatus: StatusOK}
}

func readOne(t *testing.T, r *Reader, ctx context.Context, eventID id.EventID) MessageRow {
	t.Helper()
	msgs, _, err := r.Messages(ctx, room, MessageFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.EventID == eventID {
			return m
		}
	}
	t.Fatalf("message %s not found", eventID)
	return MessageRow{}
}

func body(m MessageRow) string {
	if m.Body == nil {
		return "<nil>"
	}
	return *m.Body
}

func TestNewRoomQueuesInitialBackfillAndLimitedQueuesGap(t *testing.T) {
	w, _, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) { joinRoom(b, "t1") })
	apply(t, w, ctx, func(b *Batch) { joinRoom(b, "t2") })
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "t3")
		b.Room(room).Limited = true
	})
	tasks, err := w.NextBackfillTasks(ctx, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 2 || tasks[0].Kind != BackfillGap || tasks[0].Token != "t3" || tasks[1].Kind != BackfillInitial || tasks[1].Token != "t1" {
		t.Fatalf("unexpected tasks: %+v", tasks)
	}
	apply(t, w, ctx, func(b *Batch) { b.Backfill = &BackfillProgress{TaskID: tasks[0].ID, Done: true} })
	apply(t, w, ctx, func(b *Batch) { b.Backfill = &BackfillProgress{TaskID: tasks[1].ID, NextToken: "t0"} })
	tasks, _ = w.NextBackfillTasks(ctx, nil, 10)
	if len(tasks) != 1 || tasks[0].Token != "t0" {
		t.Fatalf("unexpected tasks after progress: %+v", tasks)
	}
	if tasks, _ = w.NextBackfillTasks(ctx, map[int64]struct{}{tasks[0].ID: {}}, 10); len(tasks) != 0 {
		t.Fatalf("excluded task returned: %+v", tasks)
	}
}

func TestEditsApplyInAnyOrder(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		b.Edits = append(b.Edits, Edit{EventID: "$edit2", Target: "$orig", Sender: alice, TS: 300, Body: "second edit"})
		b.Edits = append(b.Edits, Edit{EventID: "$edit1", Target: "$orig", Sender: alice, TS: 200, Body: "first edit"})
	})
	apply(t, w, ctx, func(b *Batch) { b.Messages = append(b.Messages, text("$orig", alice, 100, "original")) })
	m := readOne(t, r, ctx, "$orig")
	if body(m) != "second edit" || m.EditedAt == nil || *m.EditedAt != 300 {
		t.Fatalf("latest edit not applied: %s %v", body(m), m.EditedAt)
	}
	apply(t, w, ctx, func(b *Batch) {
		b.Edits = append(b.Edits, Edit{EventID: "$stale", Target: "$orig", Sender: alice, TS: 150, Body: "stale"})
		b.Edits = append(b.Edits, Edit{EventID: "$forged", Target: "$orig", Sender: me, TS: 999, Body: "forged"})
	})
	if m = readOne(t, r, ctx, "$orig"); body(m) != "second edit" {
		t.Fatalf("stale or forged edit applied: %s", body(m))
	}
	results, err := r.Search(ctx, "second", SearchFilter{})
	if err != nil || len(results) != 1 {
		t.Fatalf("edited body not searchable: %v %v", results, err)
	}
	if results, _ = r.Search(ctx, "original", SearchFilter{}); len(results) != 0 {
		t.Fatalf("pre-edit body still searchable")
	}
}

func TestRedactionBeforeAndAfterMessage(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		b.Redactions = append(b.Redactions, Redaction{EventID: "$r1", Redacts: "$early", TS: 500})
	})
	apply(t, w, ctx, func(b *Batch) {
		b.Messages = append(b.Messages, text("$early", alice, 100, "secret one"), text("$late", alice, 200, "secret two"))
	})
	apply(t, w, ctx, func(b *Batch) {
		b.Redactions = append(b.Redactions, Redaction{EventID: "$r2", Redacts: "$late", TS: 600})
	})
	for _, eventID := range []id.EventID{"$early", "$late"} {
		if m := readOne(t, r, ctx, eventID); !m.Redacted || m.Body != nil {
			t.Fatalf("%s not redacted: %+v", eventID, m)
		}
	}
	if results, _ := r.Search(ctx, "secret", SearchFilter{}); len(results) != 0 {
		t.Fatalf("redacted bodies still searchable: %d", len(results))
	}
}

func TestUndecryptableResolvedLater(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		placeholder := Message{EventID: "$enc", RoomID: room, Sender: alice, TS: 100, DecryptStatus: StatusUndecryptable}
		b.Undecryptable = append(b.Undecryptable,
			Undecryptable{Placeholder: &placeholder, EventID: "$enc", RoomID: room, Sender: alice, SessionID: "sess", Raw: []byte(`{}`), Live: true},
			Undecryptable{EventID: "$encedit", RoomID: room, Sender: alice, SessionID: "sess", Raw: []byte(`{}`)},
		)
	})
	if m := readOne(t, r, ctx, "$enc"); m.DecryptStatus != StatusUndecryptable || m.Body != nil {
		t.Fatalf("placeholder wrong: %+v", m)
	}
	missing, _ := w.MissingSessions(ctx)
	if len(missing) != 1 || !missing[0].Live {
		t.Fatalf("missing session not recorded: %+v", missing)
	}
	raws, _ := w.EncryptedEvents(ctx, "sess")
	if len(raws) != 2 {
		t.Fatalf("expected 2 stored encrypted events, got %d", len(raws))
	}

	apply(t, w, ctx, func(b *Batch) {
		b.Resolved = append(b.Resolved, "$enc")
		b.Messages = append(b.Messages, text("$enc", alice, 100, "now readable"))
	})
	if m := readOne(t, r, ctx, "$enc"); m.DecryptStatus != StatusOK || body(m) != "now readable" {
		t.Fatalf("placeholder not upgraded: %+v", m)
	}
	if missing, _ = w.MissingSessions(ctx); len(missing) != 1 {
		t.Fatal("session forgotten while an event is still undecryptable")
	}
	apply(t, w, ctx, func(b *Batch) {
		b.Resolved = append(b.Resolved, "$encedit")
		b.Discard = append(b.Discard, "$encedit")
		b.Edits = append(b.Edits, Edit{EventID: "$encedit", Target: "$enc", Sender: alice, TS: 200, Body: "edited"})
	})
	if m := readOne(t, r, ctx, "$enc"); body(m) != "edited" {
		t.Fatalf("decrypted edit not applied: %s", body(m))
	}
	if missing, _ = w.MissingSessions(ctx); len(missing) != 0 {
		t.Fatalf("session should be resolved: %+v", missing)
	}
	st, _ := r.Status(ctx)
	if st.Undecryptable != 0 || st.Messages != 1 {
		t.Fatalf("unexpected status: %+v", st)
	}
}

func TestDecryptedMessageDoesNotOverwriteEdit(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		b.Messages = append(b.Messages, text("$m", alice, 100, "original"))
		b.Edits = append(b.Edits, Edit{EventID: "$e", Target: "$m", Sender: alice, TS: 200, Body: "edited"})
	})
	apply(t, w, ctx, func(b *Batch) { b.Messages = append(b.Messages, text("$m", alice, 100, "original")) })
	if m := readOne(t, r, ctx, "$m"); body(m) != "edited" {
		t.Fatalf("duplicate delivery reverted edit: %s", body(m))
	}
}

func TestUnreadFollowsReceiptsAndOwnMessages(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		b.Messages = append(b.Messages, text("$1", alice, 100, "one"), text("$2", alice, 200, "two"), text("$3", alice, 300, "three"))
	})
	unread := func() int {
		chats, err := r.Chats(ctx, ChatFilter{})
		if err != nil || len(chats) != 1 {
			t.Fatalf("chats: %v %v", chats, err)
		}
		return chats[0].Unread
	}
	if n := unread(); n != 3 {
		t.Fatalf("expected 3 unread, got %d", n)
	}
	apply(t, w, ctx, func(b *Batch) { b.Receipts = append(b.Receipts, Receipt{RoomID: room, EventID: "$1", TS: 999999}) })
	if n := unread(); n != 2 {
		t.Fatalf("receipt should resolve to the event's ts: %d unread", n)
	}
	apply(t, w, ctx, func(b *Batch) { b.Receipts = append(b.Receipts, Receipt{RoomID: room, EventID: "$unknown"}) })
	if n := unread(); n != 2 {
		t.Fatalf("unresolvable fully-read marker changed unread: %d", n)
	}
	apply(t, w, ctx, func(b *Batch) { b.Messages = append(b.Messages, text("$mine", me, 250, "reply")) })
	if n := unread(); n != 1 {
		t.Fatalf("own message should mark earlier messages read: %d unread", n)
	}
	chats, _ := r.Chats(ctx, ChatFilter{UnreadOnly: true})
	if len(chats) != 1 {
		t.Fatal("unread filter dropped the chat")
	}
}

func TestRoomNames(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		botID := bot.String()
		b.Room(room).BridgeBot = &botID
		b.Room(room).Heroes = []id.UserID{me, bot, alice}
		b.Members = append(b.Members,
			Member{RoomID: room, UserID: alice, DisplayName: "Alice Smith", Membership: "join"},
			Member{RoomID: room, UserID: bot, DisplayName: "WhatsApp bridge bot", Membership: "join"},
		)
	})
	chat, err := r.ResolveChat(ctx, "alice")
	if err != nil || chat.Name != "Alice Smith" {
		t.Fatalf("hero name: %+v %v", chat, err)
	}
	apply(t, w, ctx, func(b *Batch) {
		name := "Family"
		b.Room(room).ExplicitName = &name
	})
	if chat, _ = r.ResolveChat(ctx, room.String()); chat.Name != "Family" {
		t.Fatalf("explicit name not used: %q", chat.Name)
	}
}

func TestResolveChat(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		for roomID, name := range map[id.RoomID]string{"!a:x": "Project", "!b:x": "Project Updates", "!c:x": "Ops", "!d:x": "Ops Alerts"} {
			ru := b.Room(roomID)
			n := name
			ru.ExplicitName = &n
		}
	})
	if chat, err := r.ResolveChat(ctx, "project"); err != nil || chat.RoomID != "!a:x" {
		t.Fatalf("exact match should win: %+v %v", chat, err)
	}
	if chat, err := r.ResolveChat(ctx, "updates"); err != nil || chat.RoomID != "!b:x" {
		t.Fatalf("unique partial match: %+v %v", chat, err)
	}
	var ambiguous *AmbiguousChatError
	if _, err := r.ResolveChat(ctx, "o"); !errors.As(err, &ambiguous) || len(ambiguous.Candidates) != 4 {
		t.Fatalf("expected ambiguity, got %v", err)
	}
	if _, err := r.ResolveChat(ctx, "nothing"); !errors.Is(err, ErrChatNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestSearchQueries(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		b.Messages = append(b.Messages,
			text("$1", alice, 100, "Deployment went fine, café is open"),
			text("$2", alice, 200, `don't forget "quotes" AND operators`),
		)
	})
	for query, want := range map[string]int{
		"deploy":           1,
		"cafe":             1,
		"don't":            1,
		`"quotes"`:         1,
		"AND OR":           0,
		"deploy café":      1,
		"forget operators": 1,
	} {
		results, err := r.Search(ctx, query, SearchFilter{})
		if err != nil || len(results) != want {
			t.Errorf("search %q: got %d results, err %v", query, len(results), err)
		}
	}
	if _, err := r.Search(ctx, "  ", SearchFilter{}); !errors.Is(err, ErrEmptyQuery) {
		t.Fatalf("expected empty query error, got %v", err)
	}
	results, err := r.Search(ctx, "fine OR quotes", SearchFilter{Raw: true})
	if err != nil || len(results) != 2 || results[0].EventID != "$2" {
		t.Fatalf("raw search: %v %v", results, err)
	}
}

func TestMessagesWindowAndReplies(t *testing.T) {
	w, r, ctx := setup(t)
	apply(t, w, ctx, func(b *Batch) {
		joinRoom(b, "")
		b.Members = append(b.Members, Member{RoomID: room, UserID: alice, DisplayName: "Alice", Membership: "join"})
		reply := text("$3", me, 300, "agreed")
		reply.ReplyTo = "$2"
		b.Messages = append(b.Messages, text("$1", alice, 100, "one"), text("$2", alice, 200, "two"), reply)
	})
	msgs, hasMore, err := r.Messages(ctx, room, MessageFilter{Limit: 2})
	if err != nil || !hasMore || len(msgs) != 2 || msgs[0].EventID != "$2" || msgs[1].EventID != "$3" {
		t.Fatalf("latest window wrong: %+v %v %v", msgs, hasMore, err)
	}
	if msgs[1].ReplyTo == nil || msgs[1].ReplyTo.SenderName != "Alice" || msgs[1].ReplyTo.Body == nil || *msgs[1].ReplyTo.Body != "two" {
		t.Fatalf("reply context missing: %+v", msgs[1].ReplyTo)
	}
	if msgs, hasMore, _ = r.Messages(ctx, room, MessageFilter{SinceTS: 150, BeforeTS: 300}); len(msgs) != 1 || hasMore {
		t.Fatalf("since/before window wrong: %+v", msgs)
	}
}

func TestWriterRefusesAnotherAccountsDatabase(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "mirror.db")
	w, err := OpenWriter(ctx, path, me)
	if err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if _, err = OpenWriter(ctx, path, alice); err == nil {
		t.Fatal("expected an error opening another account's database")
	}
}

func TestReaderIsReadOnly(t *testing.T) {
	_, r, ctx := setup(t)
	if _, err := r.db.ExecContext(ctx, "INSERT INTO sync_state (key, value) VALUES ('x', 'y')"); err == nil {
		t.Fatal("reader was able to write")
	}
}
