package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/store"
)

const (
	// How long a live event waits for its key to arrive before we go looking.
	liveKeyWait = 10 * time.Second
	// Key requests go to my other devices only for recent live messages, so a
	// big backfill doesn't flood them.
	keyRequestMaxAge = 24 * time.Hour
)

// Waits between key backup checks for one session, in multiples of the key
// retry interval: with the default 5m that's 5m, 15m, 30m, 2h, 12h, then daily.
var backupCheckBackoff = []time.Duration{1, 3, 6, 24, 144, 288}

func backupDue(ms store.MissingSession, now time.Time, interval time.Duration) bool {
	if ms.BackupChecks == 0 {
		return true
	}
	wait := backupCheckBackoff[min(ms.BackupChecks-1, len(backupCheckBackoff)-1)] * interval
	return now.Sub(time.UnixMilli(ms.BackupCheckedTS)) >= wait
}

// keyLoop retries undecryptable events when their keys arrive, and goes
// looking for missing keys in the key backup and from my other devices.
func (s *Syncer) keyLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.KeyRetryInterval)
	defer ticker.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case sessionID := <-s.receivedSessions:
			s.retrySession(ctx, sessionID)
		case sessionID := <-s.liveMissing:
			s.recoverSession(ctx, sessionID)
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Syncer) sweep(ctx context.Context) {
	sessions, err := s.store.MissingSessions(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.log.Error().Err(err).Msg("Failed to list missing sessions")
		}
		return
	}
	if len(sessions) > 0 {
		s.log.Debug().Int("sessions", len(sessions)).Msg("Looking for missing room keys")
	}
	for _, ms := range sessions {
		if ctx.Err() != nil {
			return
		}
		s.recover(ctx, ms)
	}
}

func (s *Syncer) recoverSession(ctx context.Context, sessionID id.SessionID) {
	sessions, err := s.store.MissingSessions(ctx)
	if err != nil {
		return
	}
	for _, ms := range sessions {
		if ms.SessionID == sessionID {
			s.recover(ctx, ms)
			return
		}
	}
}

// recover tries, in order: a key that's already arrived, the key backup, and
// a key request to my other devices.
func (s *Syncer) recover(ctx context.Context, ms store.MissingSession) {
	log := s.log.With().Stringer("session_id", ms.SessionID).Stringer("room_id", ms.RoomID).Logger()
	if sess, err := s.crypto.Store.GetGroupSession(ctx, ms.RoomID, ms.SessionID); err == nil && sess != nil {
		if s.retrySession(ctx, ms.SessionID) == 0 {
			return
		}
	}
	now := time.Now()
	if s.backupKey != nil && backupDue(ms, now, s.cfg.KeyRetryInterval) {
		found, err := s.fetchFromBackup(ctx, ms)
		if err != nil {
			log.Warn().Err(err).Msg("Key backup lookup failed")
		}
		if markErr := s.store.MarkBackupChecked(ctx, ms.SessionID, now.UnixMilli()); markErr != nil {
			log.Warn().Err(markErr).Msg("Failed to record key backup check")
		}
		if found && s.retrySession(ctx, ms.SessionID) == 0 {
			return
		}
	}
	if ms.Live && ms.RequestedTS == 0 && now.Sub(time.UnixMilli(ms.FirstSeenTS)) < keyRequestMaxAge {
		deviceID := ms.DeviceID
		if deviceID == "" {
			deviceID = "*"
		}
		err := s.mach.SendRoomKeyRequest(ctx, ms.RoomID, ms.SenderKey, ms.SessionID, "", map[id.UserID][]id.DeviceID{
			s.me:      {"*"},
			ms.Sender: {deviceID},
		})
		if err != nil {
			log.Warn().Err(err).Msg("Failed to send key request")
			return
		}
		log.Debug().Msg("Requested room key from other devices")
		if err = s.store.MarkKeyRequested(ctx, ms.SessionID, now.UnixMilli()); err != nil {
			log.Warn().Err(err).Msg("Failed to record key request")
		}
	}
}

func (s *Syncer) fetchFromBackup(ctx context.Context, ms store.MissingSession) (bool, error) {
	resp, err := s.cli.GetKeyBackupForRoomAndSession(ctx, s.backupVersion, ms.RoomID, ms.SessionID)
	if errors.Is(err, mautrix.MNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	sessionData, err := resp.SessionData.Decrypt(s.backupKey)
	if err != nil {
		return false, err
	}
	if _, err = s.mach.ImportRoomKeyFromBackup(ctx, s.backupVersion, ms.RoomID, ms.SessionID, sessionData); err != nil {
		return false, err
	}
	return true, nil
}

// retrySession re-decrypts the stored events for a session and returns how
// many are still undecryptable.
func (s *Syncer) retrySession(ctx context.Context, sessionID id.SessionID) int {
	raws, err := s.store.EncryptedEvents(ctx, sessionID)
	if err != nil || len(raws) == 0 {
		return 0
	}
	b := store.NewBatch()
	for _, raw := range raws {
		var evt event.Event
		if err = json.Unmarshal(raw, &evt); err != nil {
			continue
		}
		prepare(&evt, evt.RoomID, event.MessageEventType)
		s.handleEncrypted(ctx, b, &evt, sourceRetry)
	}
	if err = s.store.Apply(ctx, b); err != nil {
		s.log.Error().Err(err).Stringer("session_id", sessionID).Msg("Failed to store decrypted events")
		return len(raws)
	}
	if resolved := len(raws) - len(b.Undecryptable); resolved > 0 {
		s.log.Info().Stringer("session_id", sessionID).Int("events", resolved).Msg("Decrypted events after key arrived")
	}
	return len(b.Undecryptable)
}
