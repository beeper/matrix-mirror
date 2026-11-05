package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/sqlitedb"
)

var (
	ErrNoDatabase   = errors.New("no mirror database yet: run `matrix-mirror sync` first")
	ErrChatNotFound = errors.New("no chat matches")
	ErrEmptyQuery   = errors.New("search query is empty")
)

type AmbiguousChatError struct {
	Query      string
	Candidates []Chat
}

func (e *AmbiguousChatError) Error() string {
	return fmt.Sprintf("%q matches %d chats; use a room id or a more specific name", e.Query, len(e.Candidates))
}

// Reader is a read-only handle on mirror.db, safe to use while sync is running.
type Reader struct {
	db *sql.DB
	Me id.UserID
}

func OpenReader(ctx context.Context, path string) (*Reader, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoDatabase
	}
	db, err := sqlitedb.Open(path, true)
	if err != nil {
		return nil, err
	}
	var version int
	if err = db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		_ = db.Close()
		return nil, err
	}
	if version != len(migrations) {
		_ = db.Close()
		if version == 0 {
			return nil, ErrNoDatabase
		}
		return nil, fmt.Errorf("mirror.db schema version %d doesn't match this binary (%d): restart `matrix-mirror sync` with this version", version, len(migrations))
	}
	r := &Reader{db: db}
	if err = db.QueryRowContext(ctx, "SELECT value FROM sync_state WHERE key = 'user_id'").Scan(&r.Me); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		return nil, err
	}
	return r, nil
}

func (r *Reader) Close() error {
	return r.db.Close()
}

type Chat struct {
	RoomID      id.RoomID
	Name        string
	Network     string
	NetworkName string
	IsDM        bool
	LastEventTS int64
	MyReadTS    int64
	Membership  string
	Unread      int
}

type Reply struct {
	EventID    id.EventID
	Sender     id.UserID
	SenderName string
	Body       *string
}

type MessageRow struct {
	EventID       id.EventID
	RoomID        id.RoomID
	RoomName      string
	Sender        id.UserID
	SenderName    string
	TS            int64
	MsgType       string
	Body          *string
	ReplyTo       *Reply
	EditedAt      *int64
	Redacted      bool
	DecryptStatus string
	Attachment    *Attachment
	Snippet       string
}

const chatColumns = `r.room_id, COALESCE(r.name, ''), COALESCE(r.network, ''), COALESCE(r.network_name, ''), r.is_dm,
	COALESCE(r.last_event_ts, 0) AS last_ts, COALESCE(r.my_read_ts, 0), COALESCE(r.membership, ''),
	(SELECT COUNT(*) FROM messages m WHERE m.room_id = r.room_id AND m.ts > COALESCE(r.my_read_ts, 0)
		AND m.sender != ?1 AND m.redacted = 0) AS unread`

const visibleRooms = `COALESCE(r.room_type, '') != 'm.space' AND COALESCE(r.membership, 'join') != 'leave'`

func scanChat(rows interface{ Scan(...any) error }) (Chat, error) {
	var c Chat
	err := rows.Scan(&c.RoomID, &c.Name, &c.Network, &c.NetworkName, &c.IsDM, &c.LastEventTS, &c.MyReadTS, &c.Membership, &c.Unread)
	return c, err
}

type ChatFilter struct {
	UnreadOnly bool
	Network    string
	Limit      int
}

func (r *Reader) Chats(ctx context.Context, f ChatFilter) ([]Chat, error) {
	query := "SELECT * FROM (SELECT " + chatColumns + " FROM rooms r WHERE " + visibleRooms
	args := []any{r.Me}
	switch {
	case strings.EqualFold(f.Network, "matrix"):
		query += " AND r.network IS NULL"
	case f.Network != "":
		args = append(args, f.Network)
		query += fmt.Sprintf(" AND (r.network = ?%d COLLATE NOCASE OR r.network_name = ?%d COLLATE NOCASE)", len(args), len(args))
	}
	query += ")"
	if f.UnreadOnly {
		query += " WHERE unread > 0"
	}
	query += " ORDER BY last_ts DESC"
	if f.Limit > 0 {
		args = append(args, f.Limit)
		query += fmt.Sprintf(" LIMIT ?%d", len(args))
	}
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveChat finds a chat by room id, else by a unique exact or substring
// match on its name (case-insensitive).
func (r *Reader) ResolveChat(ctx context.Context, query string) (*Chat, error) {
	query = strings.TrimSpace(query)
	if strings.HasPrefix(query, "!") {
		row := r.db.QueryRowContext(ctx, "SELECT "+chatColumns+" FROM rooms r WHERE r.room_id = ?2", r.Me, query)
		c, err := scanChat(row)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%w %q", ErrChatNotFound, query)
		}
		return &c, err
	}
	chats, err := r.Chats(ctx, ChatFilter{})
	if err != nil {
		return nil, err
	}
	needle := strings.ToLower(query)
	var exact, partial []Chat
	for _, c := range chats {
		name := strings.ToLower(c.Name)
		switch {
		case name == needle:
			exact = append(exact, c)
		case strings.Contains(name, needle):
			partial = append(partial, c)
		}
	}
	for _, matches := range [][]Chat{exact, partial} {
		switch {
		case len(matches) == 1:
			return &matches[0], nil
		case len(matches) > 1:
			return nil, &AmbiguousChatError{Query: query, Candidates: matches}
		}
	}
	return nil, fmt.Errorf("%w %q", ErrChatNotFound, query)
}

const messageColumns = `m.event_id, m.room_id, COALESCE(r.name, ''), m.sender, COALESCE(sm.display_name, ''), m.ts,
	COALESCE(m.msgtype, ''), m.body, m.reply_to, rm.sender, COALESCE(rsm.display_name, ''), rm.body,
	m.edited_at, m.redacted, m.decrypt_status, m.attachment`

const messageJoins = `JOIN rooms r ON r.room_id = m.room_id
	LEFT JOIN members sm ON sm.room_id = m.room_id AND sm.user_id = m.sender
	LEFT JOIN messages rm ON rm.event_id = m.reply_to
	LEFT JOIN members rsm ON rsm.room_id = rm.room_id AND rsm.user_id = rm.sender`

func scanMessage(rows interface{ Scan(...any) error }, extra ...any) (MessageRow, error) {
	var m MessageRow
	var body, replyTo, replySender, replyBody, attachment sql.NullString
	var replySenderName string
	var editedAt sql.NullInt64
	dest := []any{&m.EventID, &m.RoomID, &m.RoomName, &m.Sender, &m.SenderName, &m.TS, &m.MsgType, &body, &replyTo,
		&replySender, &replySenderName, &replyBody, &editedAt, &m.Redacted, &m.DecryptStatus, &attachment}
	if err := rows.Scan(append(dest, extra...)...); err != nil {
		return m, err
	}
	if body.Valid {
		m.Body = &body.String
	}
	if replyTo.Valid {
		m.ReplyTo = &Reply{EventID: id.EventID(replyTo.String), Sender: id.UserID(replySender.String), SenderName: replySenderName}
		if replyBody.Valid {
			m.ReplyTo.Body = &replyBody.String
		}
	}
	if editedAt.Valid {
		m.EditedAt = &editedAt.Int64
	}
	if attachment.Valid {
		m.Attachment = &Attachment{}
		if err := json.Unmarshal([]byte(attachment.String), m.Attachment); err != nil {
			m.Attachment = nil
		}
	}
	return m, nil
}

type MessageFilter struct {
	SinceTS  int64
	BeforeTS int64
	Limit    int
}

// Messages returns the latest Limit messages in the window, oldest first, and
// whether older messages in the window were left out.
func (r *Reader) Messages(ctx context.Context, roomID id.RoomID, f MessageFilter) ([]MessageRow, bool, error) {
	query := "SELECT " + messageColumns + " FROM messages m " + messageJoins + " WHERE m.room_id = ?1 AND m.ts >= ?2"
	args := []any{roomID, f.SinceTS}
	if f.BeforeTS > 0 {
		query += " AND m.ts < ?3"
		args = append(args, f.BeforeTS)
	}
	query += " ORDER BY m.ts DESC, m.id DESC"
	if f.Limit > 0 {
		query += fmt.Sprintf(" LIMIT %d", f.Limit+1)
	}
	out, err := r.queryMessages(ctx, query, args, false)
	if err != nil {
		return nil, false, err
	}
	hasMore := f.Limit > 0 && len(out) > f.Limit
	if hasMore {
		out = out[:f.Limit]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, hasMore, nil
}

func (r *Reader) queryMessages(ctx context.Context, query string, args []any, withSnippet bool) ([]MessageRow, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MessageRow
	for rows.Next() {
		var snippet string
		var extra []any
		if withSnippet {
			extra = append(extra, &snippet)
		}
		m, err := scanMessage(rows, extra...)
		if err != nil {
			return nil, err
		}
		m.Snippet = snippet
		out = append(out, m)
	}
	return out, rows.Err()
}

func (r *Reader) LatestMessage(ctx context.Context, roomID id.RoomID) (*MessageRow, error) {
	msgs, _, err := r.Messages(ctx, roomID, MessageFilter{Limit: 1})
	if err != nil || len(msgs) == 0 {
		return nil, err
	}
	return &msgs[0], nil
}

type SearchFilter struct {
	RoomID  id.RoomID
	SinceTS int64
	Limit   int
	Raw     bool
}

func (r *Reader) Search(ctx context.Context, query string, f SearchFilter) ([]MessageRow, error) {
	match := query
	if !f.Raw {
		match = MatchExpression(query)
	}
	if strings.TrimSpace(match) == "" {
		return nil, ErrEmptyQuery
	}
	sqlQuery := "SELECT " + messageColumns + `, snippet(messages_fts, 0, '[', ']', '…', 12)
		FROM messages_fts JOIN messages m ON m.id = messages_fts.rowid ` + messageJoins + `
		WHERE messages_fts MATCH ?1 AND m.ts >= ?2`
	args := []any{match, f.SinceTS}
	if f.RoomID != "" {
		sqlQuery += " AND m.room_id = ?3"
		args = append(args, f.RoomID)
	}
	sqlQuery += " ORDER BY m.ts DESC"
	if f.Limit > 0 {
		sqlQuery += fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	return r.queryMessages(ctx, sqlQuery, args, true)
}

// MatchExpression turns free text into an FTS5 query: every word must appear,
// each matched as a prefix, with FTS5 operators treated as plain text.
func MatchExpression(text string) string {
	var terms []string
	for _, word := range strings.Fields(text) {
		terms = append(terms, `"`+strings.ReplaceAll(word, `"`, `""`)+`"*`)
	}
	return strings.Join(terms, " ")
}

type Status struct {
	UserID          id.UserID
	LastSyncAt      string
	Rooms           int
	Messages        int
	Undecryptable   int
	MissingSessions int
	BackfillPending int
}

func (r *Reader) Status(ctx context.Context) (*Status, error) {
	s := &Status{UserID: r.Me}
	counts := []struct {
		dest  *int
		query string
	}{
		{&s.Rooms, "SELECT COUNT(*) FROM rooms r WHERE " + visibleRooms},
		{&s.Messages, "SELECT COUNT(*) FROM messages"},
		{&s.Undecryptable, "SELECT COUNT(*) FROM messages WHERE decrypt_status = 'undecryptable'"},
		{&s.MissingSessions, "SELECT COUNT(*) FROM missing_sessions"},
		{&s.BackfillPending, "SELECT COUNT(DISTINCT room_id) FROM backfill_queue"},
	}
	for _, c := range counts {
		if err := r.db.QueryRowContext(ctx, c.query).Scan(c.dest); err != nil {
			return nil, err
		}
	}
	err := r.db.QueryRowContext(ctx, "SELECT value FROM sync_state WHERE key = 'last_sync_at'").Scan(&s.LastSyncAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	return s, nil
}
