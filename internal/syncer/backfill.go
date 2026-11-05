package syncer

import (
	"context"
	"errors"
	"math"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/store"
)

const (
	backfillPageSize   = 100
	backfillRetryDelay = 2 * time.Minute
	// An empty page with an end token means "keep going", but a long run of
	// them means the server isn't going to get anywhere.
	maxEmptyPages = 20
)

var backfillFilter = &mautrix.FilterPart{
	LazyLoadMembers: true,
	Types:           []event.Type{event.EventMessage, event.EventEncrypted, event.EventSticker, event.EventRedaction},
}

// backfillLoop drains the backfill queue: gaps from limited syncs first, then
// rooms' history in order of recent activity.
func (s *Syncer) backfillLoop(ctx context.Context) {
	if s.cfg.BackfillDays <= 0 {
		return
	}
	var mu sync.Mutex
	busy := make(map[int64]struct{})
	slots := make(chan struct{}, s.cfg.BackfillWorkers)
	var wg sync.WaitGroup
	defer wg.Wait()
	release := func(taskID int64) {
		mu.Lock()
		delete(busy, taskID)
		mu.Unlock()
		s.wakeBackfill()
	}
	for {
		mu.Lock()
		exclude := make(map[int64]struct{}, len(busy))
		for taskID := range busy {
			exclude[taskID] = struct{}{}
		}
		mu.Unlock()
		tasks, err := s.store.NextBackfillTasks(ctx, exclude, s.cfg.BackfillWorkers)
		if err != nil && ctx.Err() == nil {
			s.log.Error().Err(err).Msg("Failed to read backfill queue")
		}
		if len(tasks) == 0 {
			select {
			case <-ctx.Done():
				return
			case <-s.backfillWake:
			case <-time.After(time.Minute):
			}
			continue
		}
		for _, task := range tasks {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			mu.Lock()
			busy[task.ID] = struct{}{}
			mu.Unlock()
			wg.Add(1)
			go func(task store.BackfillTask) {
				defer wg.Done()
				defer func() { <-slots }()
				err := s.backfillRoom(ctx, task)
				if err == nil || ctx.Err() != nil {
					release(task.ID)
					return
				}
				s.log.Warn().Err(err).Stringer("room_id", task.RoomID).Msg("Backfill failed, will retry")
				time.AfterFunc(backfillRetryDelay, func() { release(task.ID) })
			}(task)
		}
	}
}

func (s *Syncer) backfillRoom(ctx context.Context, task store.BackfillTask) error {
	log := s.log.With().Stringer("room_id", task.RoomID).Str("kind", task.Kind).Logger()
	ctx = log.WithContext(ctx)
	cutoff := time.Now().AddDate(0, 0, -s.cfg.BackfillDays).UnixMilli()
	token := task.Token
	total, emptyPages := 0, 0
	for {
		resp, err := s.cli.Messages(ctx, task.RoomID, token, "", mautrix.DirectionBackward, backfillFilter, backfillPageSize)
		if errors.Is(err, mautrix.MForbidden) || errors.Is(err, mautrix.MNotFound) {
			log.Info().Err(err).Msg("Can't read room history, skipping backfill")
			b := store.NewBatch()
			b.Backfill = &store.BackfillProgress{TaskID: task.ID, Done: true}
			return s.store.Apply(ctx, b)
		} else if err != nil {
			return err
		}
		b := store.NewBatch()
		for _, evt := range resp.State {
			prepare(evt, task.RoomID, event.StateEventType)
			if member, ok := evt.Content.Parsed.(*event.MemberEventContent); ok {
				b.BackfillMembers = append(b.BackfillMembers, memberFrom(task.RoomID, id.UserID(evt.GetStateKey()), member))
			}
		}
		eventIDs := make([]id.EventID, 0, len(resp.Chunk))
		oldest := int64(math.MaxInt64)
		for _, evt := range resp.Chunk {
			prepare(evt, task.RoomID, event.MessageEventType)
			eventIDs = append(eventIDs, evt.ID)
			oldest = min(oldest, evt.Timestamp)
			if evt.StateKey == nil {
				s.handleEvent(ctx, b, evt, sourceBackfill)
			}
		}
		reachedKnown, err := s.store.KnownEvents(ctx, eventIDs)
		if err != nil {
			return err
		}
		if len(resp.Chunk) == 0 {
			emptyPages++
		} else {
			emptyPages = 0
		}
		done := resp.End == "" || resp.End == token || oldest < cutoff || reachedKnown || emptyPages >= maxEmptyPages
		b.Backfill = &store.BackfillProgress{TaskID: task.ID, NextToken: resp.End, Done: done}
		if err = s.store.Apply(ctx, b); err != nil {
			return err
		}
		total += len(b.Messages) + len(b.Undecryptable)
		if done {
			log.Info().Int("events", total).Msg("Backfilled room")
			return nil
		}
		token = resp.End
	}
}
