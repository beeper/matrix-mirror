package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/sqlitedb"
)

// Writer is the mirror.db handle owned by the sync process, the only writer.
type Writer struct {
	db *sql.DB
	me id.UserID
}

func OpenWriter(ctx context.Context, path string, me id.UserID) (*Writer, error) {
	db, err := sqlitedb.Open(path, false)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err = migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	w := &Writer{db: db, me: me}
	owner, err := w.SyncState(ctx, "user_id")
	if err == nil && owner != "" && owner != me.String() {
		err = fmt.Errorf("%s holds %s's chats, not %s's: move it aside to mirror another account", path, owner, me)
	}
	if err == nil {
		err = w.SetSyncState(ctx, "user_id", me.String())
	}
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return w, nil
}

func (w *Writer) Close() error {
	return w.db.Close()
}

func (w *Writer) SyncState(ctx context.Context, key string) (string, error) {
	var value string
	err := w.db.QueryRowContext(ctx, "SELECT value FROM sync_state WHERE key = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (w *Writer) SetSyncState(ctx context.Context, key, value string) error {
	_, err := w.db.ExecContext(ctx, upsertSyncState, key, value)
	return err
}

func (w *Writer) DeleteSyncState(ctx context.Context, keys ...string) error {
	for _, key := range keys {
		if _, err := w.db.ExecContext(ctx, "DELETE FROM sync_state WHERE key = ?", key); err != nil {
			return err
		}
	}
	return nil
}

const upsertSyncState = `INSERT INTO sync_state (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`

func (w *Writer) Apply(ctx context.Context, b *Batch) (err error) {
	tx, err := w.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	a := &applier{tx: tx, ctx: ctx, me: w.me, now: time.Now(), touched: make(map[id.RoomID]struct{})}
	if err = a.apply(b); err != nil {
		return err
	}
	return tx.Commit()
}

type applier struct {
	tx      *sql.Tx
	ctx     context.Context
	me      id.UserID
	now     time.Time
	touched map[id.RoomID]struct{}
}

func (a *applier) exec(query string, args ...any) (sql.Result, error) {
	return a.tx.ExecContext(a.ctx, query, args...)
}

func (a *applier) apply(b *Batch) error {
	for _, ru := range b.Rooms() {
		if err := a.room(ru); err != nil {
			return fmt.Errorf("room %s: %w", ru.RoomID, err)
		}
	}
	for _, m := range b.Members {
		if err := a.member(m, true); err != nil {
			return err
		}
	}
	for _, m := range b.BackfillMembers {
		if err := a.member(m, false); err != nil {
			return err
		}
	}
	for _, roomID := range b.DirectRooms {
		if _, err := a.exec("UPDATE rooms SET is_dm = 1 WHERE room_id = ?", roomID); err != nil {
			return err
		}
	}
	for _, eventID := range b.Discard {
		if _, err := a.exec("DELETE FROM messages WHERE event_id = ? AND decrypt_status = ?", eventID, StatusUndecryptable); err != nil {
			return err
		}
	}
	for i := range b.Messages {
		if err := a.message(&b.Messages[i]); err != nil {
			return fmt.Errorf("message %s: %w", b.Messages[i].EventID, err)
		}
	}
	for _, e := range b.Edits {
		if err := a.edit(e); err != nil {
			return fmt.Errorf("edit %s: %w", e.EventID, err)
		}
	}
	for _, r := range b.Redactions {
		if err := a.redaction(r); err != nil {
			return fmt.Errorf("redaction %s: %w", r.EventID, err)
		}
	}
	for i := range b.Undecryptable {
		if err := a.undecryptable(&b.Undecryptable[i]); err != nil {
			return fmt.Errorf("undecryptable %s: %w", b.Undecryptable[i].EventID, err)
		}
	}
	for _, eventID := range b.Resolved {
		if err := a.resolved(eventID); err != nil {
			return err
		}
	}
	for _, r := range b.Receipts {
		if err := a.receipt(r); err != nil {
			return err
		}
	}
	if b.Backfill != nil {
		if err := a.backfillProgress(b.Backfill); err != nil {
			return err
		}
	}
	for key, value := range b.SyncState {
		if _, err := a.exec(upsertSyncState, key, value); err != nil {
			return err
		}
	}
	for roomID := range a.touched {
		if err := a.refreshName(roomID); err != nil {
			return fmt.Errorf("room name %s: %w", roomID, err)
		}
	}
	return nil
}

func (a *applier) touch(roomID id.RoomID) {
	a.touched[roomID] = struct{}{}
}

func (a *applier) nowString() string {
	return a.now.UTC().Format(time.RFC3339)
}

func (a *applier) ensureRoom(roomID id.RoomID) (bool, error) {
	res, err := a.exec("INSERT INTO rooms (room_id, updated_at) VALUES (?, ?) ON CONFLICT(room_id) DO NOTHING", roomID, a.nowString())
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if n == 1 {
		a.touch(roomID)
	}
	return n == 1, err
}

func (a *applier) room(ru *RoomUpdate) error {
	created, err := a.ensureRoom(ru.RoomID)
	if err != nil {
		return err
	}
	if ru.PrevBatch != "" && (created || ru.Limited) {
		kind := BackfillGap
		if created {
			kind = BackfillInitial
		}
		_, err = a.exec("INSERT INTO backfill_queue (room_id, token, kind, created_ts) VALUES (?, ?, ?, ?)",
			ru.RoomID, ru.PrevBatch, kind, a.now.UnixMilli())
		if err != nil {
			return err
		}
	}

	sets := []string{"updated_at = ?"}
	args := []any{a.nowString()}
	set := func(column string, value any) {
		sets = append(sets, column+" = ?")
		args = append(args, value)
	}
	if ru.Membership != "" {
		set("membership", ru.Membership)
	}
	if ru.ExplicitName != nil {
		set("explicit_name", nullString(*ru.ExplicitName))
	}
	if ru.CanonicalAlias != nil {
		set("canonical_alias", nullString(*ru.CanonicalAlias))
	}
	if ru.Heroes != nil {
		heroes, _ := json.Marshal(ru.Heroes)
		set("heroes", string(heroes))
	}
	if ru.RoomType != nil {
		set("room_type", nullString(*ru.RoomType))
	}
	if ru.Network != nil {
		set("network", nullString(*ru.Network))
	}
	if ru.NetworkName != nil {
		set("network_name", nullString(*ru.NetworkName))
	}
	if ru.BridgeBot != nil {
		set("bridge_bot", nullString(*ru.BridgeBot))
	}
	if ru.IsDM {
		set("is_dm", 1)
	}
	if ru.Encrypted {
		set("encrypted", 1)
	}
	args = append(args, ru.RoomID)
	if _, err = a.exec("UPDATE rooms SET "+strings.Join(sets, ", ")+" WHERE room_id = ?", args...); err != nil {
		return err
	}
	a.touch(ru.RoomID)
	return nil
}

func (a *applier) member(m Member, overwrite bool) error {
	if _, err := a.ensureRoom(m.RoomID); err != nil {
		return err
	}
	query := `INSERT INTO members (room_id, user_id, display_name, membership) VALUES (?, ?, ?, ?)
		ON CONFLICT(room_id, user_id) DO UPDATE SET display_name = excluded.display_name, membership = excluded.membership`
	if !overwrite {
		query = `INSERT INTO members (room_id, user_id, display_name, membership) VALUES (?, ?, ?, ?)
			ON CONFLICT(room_id, user_id) DO NOTHING`
	}
	if _, err := a.exec(query, m.RoomID, m.UserID, nullString(m.DisplayName), nullString(m.Membership)); err != nil {
		return err
	}
	a.touch(m.RoomID)
	return nil
}

func (a *applier) message(m *Message) error {
	if _, err := a.ensureRoom(m.RoomID); err != nil {
		return err
	}
	res, err := a.exec(`
		INSERT INTO messages (event_id, room_id, sender, ts, msgtype, body, reply_to, redacted, decrypt_status, attachment)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO UPDATE SET
			msgtype = excluded.msgtype,
			body = excluded.body,
			reply_to = excluded.reply_to,
			redacted = excluded.redacted,
			decrypt_status = excluded.decrypt_status,
			attachment = excluded.attachment
		WHERE messages.decrypt_status = 'undecryptable' AND excluded.decrypt_status != 'undecryptable'`,
		m.EventID, m.RoomID, m.Sender, m.TS, nullString(m.MsgType), messageBody(m), nullString(string(m.ReplyTo)),
		m.Redacted, m.DecryptStatus, attachmentJSON(m.Attachment))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil
	}
	if err = a.applyLatestEdit(m.EventID, m.Sender); err != nil {
		return err
	}
	if _, err = a.exec(`UPDATE messages SET body = NULL, attachment = NULL, redacted = 1
		WHERE event_id = ? AND EXISTS (SELECT 1 FROM redactions WHERE redacts = ?)`, m.EventID, m.EventID); err != nil {
		return err
	}
	if _, err = a.exec("UPDATE rooms SET last_event_ts = MAX(COALESCE(last_event_ts, 0), ?) WHERE room_id = ?", m.TS, m.RoomID); err != nil {
		return err
	}
	if m.Sender == a.me {
		if err = a.advanceRead(m.RoomID, m.TS); err != nil {
			return err
		}
	}
	return nil
}

func messageBody(m *Message) any {
	if m.Redacted || m.DecryptStatus == StatusUndecryptable {
		return nil
	}
	return m.Body
}

func (a *applier) applyLatestEdit(target id.EventID, sender id.UserID) error {
	var e Edit
	var msgtype, body, attachment sql.NullString
	err := a.tx.QueryRowContext(a.ctx, `SELECT event_id, ts, msgtype, body, attachment FROM edits
		WHERE target = ? AND sender = ? ORDER BY ts DESC LIMIT 1`, target, sender).
		Scan(&e.EventID, &e.TS, &msgtype, &body, &attachment)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	_, err = a.exec(editUpdate, msgtype, body, attachment, e.TS, target, sender, e.TS)
	return err
}

const editUpdate = `UPDATE messages SET
		msgtype = COALESCE(?, msgtype),
		body = ?,
		attachment = COALESCE(?, attachment),
		edited_at = ?
	WHERE event_id = ? AND sender = ? AND redacted = 0 AND decrypt_status != 'undecryptable'
		AND (edited_at IS NULL OR edited_at < ?)`

func (a *applier) edit(e Edit) error {
	_, err := a.exec(`INSERT INTO edits (event_id, target, sender, ts, msgtype, body, attachment) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(event_id) DO NOTHING`,
		e.EventID, e.Target, e.Sender, e.TS, nullString(e.MsgType), e.Body, attachmentJSON(e.Attachment))
	if err != nil {
		return err
	}
	_, err = a.exec(editUpdate, nullString(e.MsgType), e.Body, attachmentJSON(e.Attachment), e.TS, e.Target, e.Sender, e.TS)
	return err
}

func (a *applier) redaction(r Redaction) error {
	if _, err := a.exec("INSERT INTO redactions (redacts, event_id, ts) VALUES (?, ?, ?) ON CONFLICT(redacts) DO NOTHING",
		r.Redacts, r.EventID, r.TS); err != nil {
		return err
	}
	if _, err := a.exec("UPDATE messages SET body = NULL, attachment = NULL, redacted = 1 WHERE event_id = ?", r.Redacts); err != nil {
		return err
	}
	_, err := a.exec("DELETE FROM edits WHERE event_id = ?", r.Redacts)
	return err
}

func (a *applier) undecryptable(u *Undecryptable) error {
	if _, err := a.exec(`INSERT INTO encrypted_events (event_id, room_id, session_id, raw) VALUES (?, ?, ?, ?)
		ON CONFLICT(event_id) DO NOTHING`, u.EventID, u.RoomID, u.SessionID, string(u.Raw)); err != nil {
		return err
	}
	if _, err := a.exec(`INSERT INTO missing_sessions (session_id, room_id, sender, sender_key, device_id, live, first_seen_ts)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET live = MAX(live, excluded.live)`,
		u.SessionID, u.RoomID, u.Sender, nullString(string(u.SenderKey)), nullString(string(u.DeviceID)), u.Live, a.now.UnixMilli()); err != nil {
		return err
	}
	if u.Placeholder != nil {
		return a.message(u.Placeholder)
	}
	return nil
}

func (a *applier) resolved(eventID id.EventID) error {
	var sessionID string
	err := a.tx.QueryRowContext(a.ctx, "SELECT session_id FROM encrypted_events WHERE event_id = ?", eventID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	if _, err = a.exec("DELETE FROM encrypted_events WHERE event_id = ?", eventID); err != nil {
		return err
	}
	_, err = a.exec(`DELETE FROM missing_sessions WHERE session_id = ?
		AND NOT EXISTS (SELECT 1 FROM encrypted_events WHERE session_id = ?)`, sessionID, sessionID)
	return err
}

func (a *applier) receipt(r Receipt) error {
	var ts sql.NullInt64
	err := a.tx.QueryRowContext(a.ctx, "SELECT ts FROM messages WHERE event_id = ?", r.EventID).Scan(&ts)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	readTS := r.TS
	if ts.Valid {
		readTS = ts.Int64
	}
	if readTS == 0 {
		return nil
	}
	return a.advanceRead(r.RoomID, readTS)
}

func (a *applier) advanceRead(roomID id.RoomID, ts int64) error {
	_, err := a.exec("UPDATE rooms SET my_read_ts = MAX(COALESCE(my_read_ts, 0), ?) WHERE room_id = ?", ts, roomID)
	return err
}

func (a *applier) backfillProgress(p *BackfillProgress) error {
	if p.Done {
		_, err := a.exec("DELETE FROM backfill_queue WHERE id = ?", p.TaskID)
		return err
	}
	_, err := a.exec("UPDATE backfill_queue SET token = ? WHERE id = ?", p.NextToken, p.TaskID)
	return err
}

const maxNameMembers = 4

func (a *applier) refreshName(roomID id.RoomID) error {
	var explicit, alias, heroesJSON, bridgeBot sql.NullString
	err := a.tx.QueryRowContext(a.ctx, "SELECT explicit_name, canonical_alias, heroes, bridge_bot FROM rooms WHERE room_id = ?", roomID).
		Scan(&explicit, &alias, &heroesJSON, &bridgeBot)
	if err != nil {
		return err
	}
	name := explicit.String
	if name == "" {
		name = alias.String
	}
	if name == "" {
		if name, err = a.memberName(roomID, heroesJSON.String, id.UserID(bridgeBot.String)); err != nil {
			return err
		}
	}
	_, err = a.exec("UPDATE rooms SET name = ? WHERE room_id = ?", nullString(name), roomID)
	return err
}

func (a *applier) memberName(roomID id.RoomID, heroesJSON string, bridgeBot id.UserID) (string, error) {
	var candidates []id.UserID
	if heroesJSON != "" {
		_ = json.Unmarshal([]byte(heroesJSON), &candidates)
	}
	if len(candidates) == 0 {
		rows, err := a.tx.QueryContext(a.ctx, `SELECT user_id FROM members WHERE room_id = ? AND membership = 'join'
			ORDER BY user_id LIMIT ?`, roomID, maxNameMembers+2)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var userID id.UserID
			if err = rows.Scan(&userID); err != nil {
				_ = rows.Close()
				return "", err
			}
			candidates = append(candidates, userID)
		}
		if err = rows.Close(); err != nil {
			return "", err
		}
	}
	var names []string
	for _, userID := range candidates {
		if userID == a.me || userID == bridgeBot || len(names) == maxNameMembers {
			continue
		}
		var displayName sql.NullString
		err := a.tx.QueryRowContext(a.ctx, "SELECT display_name FROM members WHERE room_id = ? AND user_id = ?", roomID, userID).Scan(&displayName)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		if displayName.String != "" {
			names = append(names, displayName.String)
		} else {
			names = append(names, userID.Localpart())
		}
	}
	return strings.Join(names, ", "), nil
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func attachmentJSON(att *Attachment) any {
	if att == nil {
		return nil
	}
	data, _ := json.Marshal(att)
	return string(data)
}

func (w *Writer) NextBackfillTasks(ctx context.Context, exclude map[int64]struct{}, limit int) ([]BackfillTask, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT q.id, q.room_id, q.token, q.kind FROM backfill_queue q
		LEFT JOIN rooms r ON r.room_id = q.room_id
		ORDER BY q.kind = 'gap' DESC, COALESCE(r.last_event_ts, 0) DESC, q.id
		LIMIT ?`, limit+len(exclude))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tasks []BackfillTask
	for rows.Next() {
		var t BackfillTask
		if err = rows.Scan(&t.ID, &t.RoomID, &t.Token, &t.Kind); err != nil {
			return nil, err
		}
		if _, skip := exclude[t.ID]; skip || len(tasks) == limit {
			continue
		}
		tasks = append(tasks, t)
	}
	return tasks, rows.Err()
}

func (w *Writer) KnownEvents(ctx context.Context, eventIDs []id.EventID) (bool, error) {
	for _, eventID := range eventIDs {
		var exists bool
		err := w.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM messages WHERE event_id = ?1)
			OR EXISTS (SELECT 1 FROM encrypted_events WHERE event_id = ?1)`, eventID).Scan(&exists)
		if err != nil {
			return false, err
		}
		if exists {
			return true, nil
		}
	}
	return false, nil
}

func (w *Writer) MissingSessions(ctx context.Context) ([]MissingSession, error) {
	rows, err := w.db.QueryContext(ctx, `SELECT session_id, room_id, sender, COALESCE(sender_key, ''), COALESCE(device_id, ''),
		live, first_seen_ts, COALESCE(backup_checked_ts, 0), backup_checks, COALESCE(requested_ts, 0)
		FROM missing_sessions ORDER BY first_seen_ts DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MissingSession
	for rows.Next() {
		var s MissingSession
		if err = rows.Scan(&s.SessionID, &s.RoomID, &s.Sender, &s.SenderKey, &s.DeviceID, &s.Live, &s.FirstSeenTS,
			&s.BackupCheckedTS, &s.BackupChecks, &s.RequestedTS); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (w *Writer) EncryptedEvents(ctx context.Context, sessionID id.SessionID) ([]json.RawMessage, error) {
	rows, err := w.db.QueryContext(ctx, "SELECT raw FROM encrypted_events WHERE session_id = ?", sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []json.RawMessage
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(raw))
	}
	return out, rows.Err()
}

func (w *Writer) MarkBackupChecked(ctx context.Context, sessionID id.SessionID, ts int64) error {
	_, err := w.db.ExecContext(ctx, `UPDATE missing_sessions SET backup_checked_ts = ?, backup_checks = backup_checks + 1
		WHERE session_id = ?`, ts, sessionID)
	return err
}

func (w *Writer) MarkKeyRequested(ctx context.Context, sessionID id.SessionID, ts int64) error {
	_, err := w.db.ExecContext(ctx, "UPDATE missing_sessions SET requested_ts = ? WHERE session_id = ?", ts, sessionID)
	return err
}
