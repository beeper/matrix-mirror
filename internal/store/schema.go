package store

import (
	"context"
	"database/sql"
	"fmt"
)

// Each entry upgrades the schema by one version, tracked in PRAGMA user_version.
var migrations = []string{
	`
CREATE TABLE rooms (
  room_id         TEXT PRIMARY KEY,
  name            TEXT,
  network         TEXT,
  is_dm           INTEGER NOT NULL DEFAULT 0,
  last_event_ts   INTEGER,
  my_read_ts      INTEGER,
  updated_at      TEXT NOT NULL,
  network_name    TEXT,
  explicit_name   TEXT,
  canonical_alias TEXT,
  heroes          TEXT,
  room_type       TEXT,
  bridge_bot      TEXT,
  membership      TEXT,
  encrypted       INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE members (
  room_id      TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  display_name TEXT,
  membership   TEXT,
  PRIMARY KEY (room_id, user_id)
);

CREATE TABLE messages (
  id             INTEGER PRIMARY KEY,
  event_id       TEXT NOT NULL UNIQUE,
  room_id        TEXT NOT NULL REFERENCES rooms(room_id),
  sender         TEXT NOT NULL,
  ts             INTEGER NOT NULL,
  msgtype        TEXT,
  body           TEXT,
  reply_to       TEXT,
  edited_at      INTEGER,
  redacted       INTEGER NOT NULL DEFAULT 0,
  decrypt_status TEXT NOT NULL,
  attachment     TEXT
);
CREATE INDEX messages_room_ts ON messages(room_id, ts);
CREATE INDEX messages_ts ON messages(ts);
CREATE INDEX messages_undecryptable ON messages(decrypt_status) WHERE decrypt_status = 'undecryptable';

CREATE VIRTUAL TABLE messages_fts USING fts5(
  body,
  content = 'messages',
  content_rowid = 'id',
  tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER messages_fts_insert AFTER INSERT ON messages BEGIN
  INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER messages_fts_delete AFTER DELETE ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER messages_fts_update AFTER UPDATE OF body ON messages BEGIN
  INSERT INTO messages_fts(messages_fts, rowid, body) VALUES ('delete', old.id, old.body);
  INSERT INTO messages_fts(rowid, body) VALUES (new.id, new.body);
END;

CREATE TABLE edits (
  event_id   TEXT PRIMARY KEY,
  target     TEXT NOT NULL,
  sender     TEXT NOT NULL,
  ts         INTEGER NOT NULL,
  msgtype    TEXT,
  body       TEXT,
  attachment TEXT
);
CREATE INDEX edits_target ON edits(target);

CREATE TABLE redactions (
  redacts  TEXT PRIMARY KEY,
  event_id TEXT NOT NULL,
  ts       INTEGER NOT NULL
);

CREATE TABLE encrypted_events (
  event_id   TEXT PRIMARY KEY,
  room_id    TEXT NOT NULL,
  session_id TEXT NOT NULL,
  raw        TEXT NOT NULL
);
CREATE INDEX encrypted_events_session ON encrypted_events(session_id);

CREATE TABLE missing_sessions (
  session_id        TEXT PRIMARY KEY,
  room_id           TEXT NOT NULL,
  sender            TEXT NOT NULL,
  sender_key        TEXT,
  device_id         TEXT,
  live              INTEGER NOT NULL DEFAULT 0,
  first_seen_ts     INTEGER NOT NULL,
  backup_checked_ts INTEGER,
  backup_checks     INTEGER NOT NULL DEFAULT 0,
  requested_ts      INTEGER
);

CREATE TABLE backfill_queue (
  id         INTEGER PRIMARY KEY,
  room_id    TEXT NOT NULL,
  token      TEXT NOT NULL,
  kind       TEXT NOT NULL,
  created_ts INTEGER NOT NULL
);
CREATE INDEX backfill_queue_room ON backfill_queue(room_id);

CREATE TABLE sync_state (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
`,
}

func migrate(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if version > len(migrations) {
		return fmt.Errorf("mirror.db schema version %d is newer than this binary supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, migrations[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("migrate to version %d: %w", i+1, err)
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}
