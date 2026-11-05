package syncer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/mxcrypto"
	"github.com/beeper/matrix-mirror/internal/store"
)

type Config struct {
	BackfillDays     int
	BackfillWorkers  int
	KeyRetryInterval time.Duration
}

type Syncer struct {
	cli    *mautrix.Client
	crypto *mxcrypto.Crypto
	mach   *crypto.OlmMachine
	store  *store.Writer
	log    zerolog.Logger
	cfg    Config
	me     id.UserID

	backupKey     *backup.MegolmBackupKey
	backupVersion id.KeyBackupVersion

	receivedSessions chan id.SessionID
	liveMissing      chan id.SessionID
	backfillWake     chan struct{}
}

func New(cli *mautrix.Client, c *mxcrypto.Crypto, w *store.Writer, log zerolog.Logger, cfg Config) *Syncer {
	if cfg.BackfillWorkers <= 0 {
		cfg.BackfillWorkers = 3
	}
	if cfg.KeyRetryInterval <= 0 {
		cfg.KeyRetryInterval = 5 * time.Minute
	}
	// Sender trust levels aren't used, and computing them means a key query
	// per sender, which for bridged chats is thousands of ghost users.
	c.Machine.DisableDecryptKeyFetching = true
	c.Machine.IgnorePostDecryptionParseErrors = true
	return &Syncer{
		cli:              cli,
		crypto:           c,
		mach:             c.Machine,
		store:            w,
		log:              log,
		cfg:              cfg,
		me:               cli.UserID,
		receivedSessions: make(chan id.SessionID, 4096),
		liveMissing:      make(chan id.SessionID, 4096),
		backfillWake:     make(chan struct{}, 1),
	}
}

func (s *Syncer) Run(ctx context.Context) error {
	ctx = s.log.WithContext(ctx)
	s.mach.SessionReceived = func(_ context.Context, _ id.RoomID, sessionID id.SessionID, _ uint32) {
		select {
		case s.receivedSessions <- sessionID:
		default:
		}
	}
	s.loadBackup(ctx)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.backfillLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		s.keyLoop(ctx)
	}()
	err := s.syncLoop(ctx)
	cancel()
	wg.Wait()
	return err
}

func (s *Syncer) loadBackup(ctx context.Context) {
	key, err := s.crypto.BackupKey(ctx)
	if err != nil {
		s.log.Warn().Err(err).Msg("Failed to load key backup key")
		return
	} else if key == nil {
		s.log.Warn().Msg("No key backup key: run `matrix-mirror verify` so history can be decrypted")
		return
	}
	version, err := s.mach.GetAndVerifyLatestKeyBackupVersion(ctx, key)
	if err != nil || version == nil {
		s.log.Warn().Err(err).Msg("Key backup isn't usable with the saved key; run `matrix-mirror verify` again")
		return
	}
	s.backupKey, s.backupVersion = key, version.Version
	s.log.Info().Stringer("version", version.Version).Msg("Using key backup")
}

const filterVersion = "1"

var syncFilter = &mautrix.Filter{
	Presence:    &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
	AccountData: &mautrix.FilterPart{Types: []event.Type{event.AccountDataDirectChats}},
	Room: &mautrix.RoomFilter{
		State:       &mautrix.FilterPart{LazyLoadMembers: true},
		Timeline:    &mautrix.FilterPart{Limit: 20, LazyLoadMembers: true},
		Ephemeral:   &mautrix.FilterPart{Types: []event.Type{event.EphemeralEventReceipt}},
		AccountData: &mautrix.FilterPart{Types: []event.Type{event.AccountDataFullyRead}},
	},
}

func (s *Syncer) filterID(ctx context.Context) (string, error) {
	filterID, err := s.store.SyncState(ctx, "filter_id")
	if err != nil {
		return "", err
	}
	version, err := s.store.SyncState(ctx, "filter_version")
	if err != nil {
		return "", err
	}
	if filterID != "" && version == filterVersion {
		return filterID, nil
	}
	resp, err := s.cli.CreateFilter(ctx, syncFilter)
	if err != nil {
		return "", fmt.Errorf("create sync filter: %w", err)
	}
	if err = s.store.SetSyncState(ctx, "filter_id", resp.FilterID); err != nil {
		return "", err
	}
	return resp.FilterID, s.store.SetSyncState(ctx, "filter_version", filterVersion)
}

const (
	syncTimeout = 30 * time.Second
	// A first sync of a large account is one huge response that can take
	// minutes to generate, longer than the client's usual request timeout.
	initialSyncHTTPTimeout = 30 * time.Minute
)

func (s *Syncer) syncLoop(ctx context.Context) error {
	since, err := s.store.SyncState(ctx, "next_batch")
	if err != nil {
		return err
	}
	var filterID string
	failures := 0
	for {
		if filterID == "" {
			if filterID, err = s.filterID(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return err
			}
		}
		req := mautrix.ReqSync{
			Timeout:     int(syncTimeout.Milliseconds()),
			Since:       since,
			FilterID:    filterID,
			SetPresence: event.PresenceOffline,
		}
		if since == "" || failures > 0 {
			req.Timeout = 0
		}
		if since == "" {
			req.Client = &http.Client{Timeout: initialSyncHTTPTimeout, Transport: s.cli.Client.Transport}
			s.log.Info().Msg("Starting initial sync, which can take a few minutes on large accounts")
		}
		start := time.Now()
		resp, err := s.cli.FullSyncRequest(ctx, req)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, mautrix.MUnknownToken) {
				return fmt.Errorf("the homeserver rejected the access token (was this device logged out?): %w", err)
			}
			failures++
			wait := min(time.Duration(1<<min(failures, 6))*time.Second, time.Minute)
			s.log.Warn().Err(err).Int("failures", failures).Dur("retry_in", wait).Msg("Sync failed")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(wait):
			}
			continue
		}
		failures = 0

		s.mach.ProcessSyncResponse(ctx, resp, since)
		b := s.processSync(ctx, resp)
		b.SyncState["next_batch"] = resp.NextBatch
		b.SyncState["last_sync_at"] = time.Now().UTC().Format(time.RFC3339)
		if err = s.store.Apply(ctx, b); err != nil {
			return fmt.Errorf("write sync response: %w", err)
		}
		s.logSync(b, since == "", time.Since(start))
		for _, u := range b.Undecryptable {
			s.queueLiveMissing(u.SessionID)
		}
		since = resp.NextBatch
		s.wakeBackfill()
	}
}

func (s *Syncer) logSync(b *store.Batch, initial bool, took time.Duration) {
	level := zerolog.DebugLevel
	if initial || len(b.Messages)+len(b.Edits)+len(b.Redactions)+len(b.Undecryptable) > 0 {
		level = zerolog.InfoLevel
	}
	msg := "Synced"
	if initial {
		msg = "Initial sync done"
	}
	s.log.WithLevel(level).
		Int("rooms", len(b.Rooms())).
		Int("messages", len(b.Messages)).
		Int("edits", len(b.Edits)).
		Int("redactions", len(b.Redactions)).
		Int("undecryptable", len(b.Undecryptable)).
		Dur("took", took).
		Msg(msg)
}

func (s *Syncer) processSync(ctx context.Context, resp *mautrix.RespSync) *store.Batch {
	b := store.NewBatch()
	for _, evt := range resp.AccountData.Events {
		prepare(evt, "", event.AccountDataEventType)
		if direct, ok := evt.Content.Parsed.(*event.DirectChatsEventContent); ok {
			for _, rooms := range *direct {
				b.DirectRooms = append(b.DirectRooms, rooms...)
			}
		}
	}
	for roomID, room := range resp.Rooms.Join {
		s.processRoom(ctx, b, roomID, store.MembershipJoin, room.Summary, room.State.Events, room.Timeline)
		for _, evt := range room.Ephemeral.Events {
			prepare(evt, roomID, event.EphemeralEventType)
			if receipts, ok := evt.Content.Parsed.(*event.ReceiptEventContent); ok {
				s.addReceipts(b, roomID, receipts)
			}
		}
		for _, evt := range room.AccountData.Events {
			prepare(evt, roomID, event.AccountDataEventType)
			if fullyRead, ok := evt.Content.Parsed.(*event.FullyReadEventContent); ok {
				b.Receipts = append(b.Receipts, store.Receipt{RoomID: roomID, EventID: fullyRead.EventID})
			}
		}
	}
	for roomID, room := range resp.Rooms.Leave {
		s.processRoom(ctx, b, roomID, store.MembershipLeave, room.Summary, room.State.Events, room.Timeline)
	}
	return b
}

func (s *Syncer) processRoom(ctx context.Context, b *store.Batch, roomID id.RoomID, membership string, summary mautrix.LazyLoadSummary, state []*event.Event, timeline mautrix.SyncTimeline) {
	ru := b.Room(roomID)
	ru.Membership = membership
	if len(summary.Heroes) > 0 {
		ru.Heroes = summary.Heroes
	}
	ru.PrevBatch = timeline.PrevBatch
	ru.Limited = timeline.Limited
	for _, evt := range state {
		prepare(evt, roomID, event.StateEventType)
		s.applyState(ctx, b, ru, evt)
	}
	for _, evt := range timeline.Events {
		prepare(evt, roomID, event.MessageEventType)
		if evt.StateKey != nil {
			s.applyState(ctx, b, ru, evt)
			continue
		}
		s.handleEvent(ctx, b, evt, sourceLive)
	}
}

func (s *Syncer) wakeBackfill() {
	select {
	case s.backfillWake <- struct{}{}:
	default:
	}
}

func (s *Syncer) queueLiveMissing(sessionID id.SessionID) {
	time.AfterFunc(liveKeyWait, func() {
		select {
		case s.liveMissing <- sessionID:
		default:
		}
	})
}
