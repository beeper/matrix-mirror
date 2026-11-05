package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/lock"
	"github.com/beeper/matrix-mirror/internal/service"
	"github.com/beeper/matrix-mirror/internal/session"
	"github.com/beeper/matrix-mirror/internal/store"
)

var errInvalidArgument = errors.New("invalid argument")

func (a *app) openReader(ctx context.Context) (*store.Reader, error) {
	return store.OpenReader(ctx, a.dir.MirrorDB())
}

func (a *app) statusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the login, last sync and what's mirrored",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			sess, err := a.loadSession()
			if err != nil && !errors.Is(err, session.ErrNotLoggedIn) {
				return err
			}
			running := lock.Held(a.dir.Lock())
			var st *store.Status
			r, err := a.openReader(ctx)
			if err == nil {
				defer r.Close()
				if st, err = r.Status(ctx); err != nil {
					return err
				}
			} else if !errors.Is(err, store.ErrNoDatabase) {
				return err
			}
			if a.jsonOut {
				return writeJSON(os.Stdout, statusJSON(sess, st, running))
			}
			printStatus(sess, st, running)
			return nil
		},
	}
	a.addJSONFlag(cmd)
	return cmd
}

func statusJSON(sess *session.Session, st *store.Status, running bool) map[string]any {
	out := map[string]any{
		"logged_in":  sess != nil,
		"user_id":    nil,
		"device_id":  nil,
		"homeserver": nil,
		"verified":   false,
		"service":    map[string]any{"installed": service.Installed()},
		"sync":       map[string]any{"running": running, "last_sync_at": nil},
	}
	if sess != nil {
		out["user_id"], out["device_id"], out["homeserver"] = sess.UserID, sess.DeviceID, sess.Homeserver
		out["verified"] = sess.VerifiedAt != nil
	}
	if st != nil {
		if st.LastSyncAt != "" {
			out["sync"].(map[string]any)["last_sync_at"] = st.LastSyncAt
		}
		out["rooms"] = st.Rooms
		out["messages"] = st.Messages
		out["undecryptable"] = st.Undecryptable
		out["missing_keys"] = st.MissingSessions
		out["backfill_pending"] = st.BackfillPending
	}
	return out
}

func printStatus(sess *session.Session, st *store.Status, running bool) {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()
	if sess == nil {
		fmt.Fprintln(w, "Account:\tnot logged in")
	} else {
		verified := "not verified (run `matrix-mirror verify`)"
		if sess.VerifiedAt != nil {
			verified = "verified"
		}
		fmt.Fprintf(w, "Account:\t%s\n", sess.UserID)
		fmt.Fprintf(w, "Device:\t%s, %s\n", sess.DeviceID, verified)
		fmt.Fprintf(w, "Homeserver:\t%s\n", sess.Homeserver)
	}
	syncState := "not running"
	if running {
		syncState = "running"
	}
	if service.Installed() {
		syncState += " (" + service.Kind + " installed)"
	}
	fmt.Fprintf(w, "Sync:\t%s\n", syncState)
	if st == nil {
		fmt.Fprintln(w, "Mirror:\tempty (run `matrix-mirror sync`)")
		return
	}
	lastSync := "never"
	if t, err := time.Parse(time.RFC3339, st.LastSyncAt); err == nil {
		lastSync = fmt.Sprintf("%s (%s ago)", t.Local().Format("2006-01-02 15:04:05"), time.Since(t).Round(time.Second))
	}
	fmt.Fprintf(w, "Last sync:\t%s\n", lastSync)
	fmt.Fprintf(w, "Chats:\t%d\n", st.Rooms)
	fmt.Fprintf(w, "Messages:\t%d\n", st.Messages)
	fmt.Fprintf(w, "Undecryptable:\t%d (%d missing keys)\n", st.Undecryptable, st.MissingSessions)
	fmt.Fprintf(w, "Backfill pending:\t%d chats\n", st.BackfillPending)
}

func (a *app) chatsCommand() *cobra.Command {
	var filter store.ChatFilter
	cmd := &cobra.Command{
		Use:   "chats",
		Short: "List chats, most recently active first",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			r, err := a.openReader(ctx)
			if err != nil {
				return err
			}
			defer r.Close()
			chats, err := r.Chats(ctx, filter)
			if err != nil {
				return err
			}
			if a.jsonOut {
				out := make([]jsonChat, 0, len(chats))
				for _, c := range chats {
					out = append(out, toJSONChat(c))
				}
				return writeJSON(os.Stdout, map[string]any{"chats": out})
			}
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			defer w.Flush()
			fmt.Fprintln(w, "CHAT\tNETWORK\tUNREAD\tLAST\tROOM ID")
			for _, c := range chats {
				unread := ""
				if c.Unread > 0 {
					unread = strconv.Itoa(c.Unread)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", chatLabel(c), networkLabel(c), unread, shortTime(c.LastEventTS), c.RoomID)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&filter.UnreadOnly, "unread", false, "only chats with unread messages")
	cmd.Flags().StringVar(&filter.Network, "network", "", "only chats on this network, e.g. whatsapp (\"matrix\" for plain Matrix rooms)")
	cmd.Flags().IntVar(&filter.Limit, "limit", 50, "maximum number of chats (0 for all)")
	a.addJSONFlag(cmd)
	return cmd
}

func (a *app) readCommand() *cobra.Command {
	var since, before string
	var limit int
	cmd := &cobra.Command{
		Use:   "read <chat>",
		Short: "Show a chat's latest messages (chat is a room id or a unique name match)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			filter := store.MessageFilter{Limit: limit}
			var err error
			if filter.SinceTS, err = parseTimeFlag("since", since); err != nil {
				return err
			}
			if filter.BeforeTS, err = parseTimeFlag("before", before); err != nil {
				return err
			}
			r, err := a.openReader(ctx)
			if err != nil {
				return err
			}
			defer r.Close()
			chat, err := r.ResolveChat(ctx, args[0])
			if err != nil {
				return err
			}
			msgs, hasMore, err := r.Messages(ctx, chat.RoomID, filter)
			if err != nil {
				return err
			}
			if a.jsonOut {
				out := make([]jsonMessage, 0, len(msgs))
				for _, m := range msgs {
					out = append(out, toJSONMessage(m, r.Me))
				}
				return writeJSON(os.Stdout, map[string]any{"chat": toJSONChat(*chat), "messages": out, "has_more": hasMore})
			}
			fmt.Printf("%s · %s · %s\n", chatLabel(*chat), networkLabel(*chat), chat.RoomID)
			if hasMore {
				fmt.Println("(older messages left out: raise --limit or use --before)")
			}
			for _, m := range msgs {
				fmt.Printf("[%s] %s: %s\n", longTime(m.TS), senderLabel(m.Sender, m.SenderName), messageText(m))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "only messages at or after this time (ISO 8601 date/time, or a duration like 24h or 7d)")
	cmd.Flags().StringVar(&before, "before", "", "only messages before this time")
	cmd.Flags().IntVar(&limit, "limit", 50, "maximum number of messages, latest first (0 for all)")
	a.addJSONFlag(cmd)
	return cmd
}

func (a *app) searchCommand() *cobra.Command {
	var chatQuery, since string
	var limit int
	var raw bool
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Full-text search across messages, newest first",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			query := strings.Join(args, " ")
			filter := store.SearchFilter{Limit: limit, Raw: raw}
			var err error
			if filter.SinceTS, err = parseTimeFlag("since", since); err != nil {
				return err
			}
			r, err := a.openReader(ctx)
			if err != nil {
				return err
			}
			defer r.Close()
			if chatQuery != "" {
				chat, err := r.ResolveChat(ctx, chatQuery)
				if err != nil {
					return err
				}
				filter.RoomID = chat.RoomID
			}
			results, err := r.Search(ctx, query, filter)
			if err != nil {
				return err
			}
			if a.jsonOut {
				out := make([]jsonSearchResult, 0, len(results))
				for _, m := range results {
					out = append(out, jsonSearchResult{jsonMessage: toJSONMessage(m, r.Me), ChatName: m.RoomName, Snippet: m.Snippet})
				}
				return writeJSON(os.Stdout, map[string]any{"query": query, "results": out})
			}
			for _, m := range results {
				fmt.Printf("[%s] %s › %s: %s\n", longTime(m.TS), orRoomID(m.RoomName, m.RoomID), senderLabel(m.Sender, m.SenderName), oneLine(m.Snippet))
			}
			if len(results) == 0 {
				fmt.Fprintln(os.Stderr, "No matches.")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&chatQuery, "chat", "", "only search this chat (room id or unique name match)")
	cmd.Flags().StringVar(&since, "since", "", "only messages at or after this time")
	cmd.Flags().IntVar(&limit, "limit", 20, "maximum number of results (0 for all)")
	cmd.Flags().BoolVar(&raw, "raw", false, "pass the query to SQLite FTS5 as-is (AND/OR/NOT, \"phrases\", prefix*)")
	a.addJSONFlag(cmd)
	return cmd
}

func (a *app) unreadCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unread",
		Short: "Summarise unread messages across all chats",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			r, err := a.openReader(ctx)
			if err != nil {
				return err
			}
			defer r.Close()
			chats, err := r.Chats(ctx, store.ChatFilter{UnreadOnly: true})
			if err != nil {
				return err
			}
			total := 0
			out := make([]jsonUnreadChat, 0, len(chats))
			w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			for _, c := range chats {
				total += c.Unread
				latest, err := r.LatestMessage(ctx, c.RoomID)
				if err != nil {
					return err
				}
				if a.jsonOut {
					entry := jsonUnreadChat{jsonChat: toJSONChat(c)}
					if latest != nil {
						msg := toJSONMessage(*latest, r.Me)
						entry.Latest = &msg
					}
					out = append(out, entry)
					continue
				}
				preview := ""
				if latest != nil {
					preview = senderLabel(latest.Sender, latest.SenderName) + ": " + oneLine(messageText(*latest))
				}
				fmt.Fprintf(w, "%s\t%s\t%d\t%s\t%s\n", chatLabel(c), networkLabel(c), c.Unread, shortTime(c.LastEventTS), truncate(preview, 80))
			}
			if a.jsonOut {
				return writeJSON(os.Stdout, map[string]any{"total_unread": total, "chats": out})
			}
			_ = w.Flush()
			fmt.Printf("%d unread in %d chats\n", total, len(chats))
			return nil
		},
	}
	a.addJSONFlag(cmd)
	return cmd
}

func chatLabel(c store.Chat) string {
	return orRoomID(c.Name, c.RoomID)
}

func orRoomID(name string, roomID id.RoomID) string {
	if name == "" {
		return roomID.String()
	}
	return name
}

func networkLabel(c store.Chat) string {
	network := c.NetworkName
	if network == "" {
		network = c.Network
	}
	if network == "" {
		network = "matrix"
	}
	if c.IsDM {
		network += " dm"
	}
	return network
}

func senderLabel(userID id.UserID, name string) string {
	if name != "" {
		return name
	}
	return userID.String()
}

func messageText(m store.MessageRow) string {
	switch {
	case m.Redacted:
		return "[deleted]"
	case m.DecryptStatus == store.StatusUndecryptable:
		return "[undecryptable]"
	}
	var parts []string
	if m.ReplyTo != nil {
		quote := "a message"
		if m.ReplyTo.Body != nil {
			quote = fmt.Sprintf("%s: %q", senderLabel(m.ReplyTo.Sender, m.ReplyTo.SenderName), truncate(oneLine(*m.ReplyTo.Body), 40))
		}
		parts = append(parts, "↪ "+quote+" ·")
	}
	body := ""
	if m.Body != nil {
		body = *m.Body
	}
	if m.Attachment != nil {
		kind := strings.TrimPrefix(m.MsgType, "m.")
		if body == m.Attachment.Filename || body == "" {
			body = fmt.Sprintf("[%s: %s]", kind, m.Attachment.Filename)
		} else {
			body = fmt.Sprintf("[%s: %s] %s", kind, m.Attachment.Filename, body)
		}
	}
	if m.MsgType == "m.emote" {
		body = "* " + body
	}
	parts = append(parts, body)
	if m.EditedAt != nil {
		parts = append(parts, "(edited)")
	}
	return strings.Join(parts, " ")
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func truncate(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func shortTime(ts int64) string {
	if ts == 0 {
		return ""
	}
	t := time.UnixMilli(ts).Local()
	now := time.Now()
	switch {
	case t.YearDay() == now.YearDay() && t.Year() == now.Year():
		return t.Format("15:04")
	case t.Year() == now.Year():
		return t.Format("Jan 2 15:04")
	}
	return t.Format("2006-01-02")
}

func longTime(ts int64) string {
	return time.UnixMilli(ts).Local().Format("2006-01-02 15:04")
}

var timeLayouts = []string{
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

// parseTimeFlag accepts ISO 8601 times (local time when no zone is given) or a
// duration back from now such as 90m, 24h, 7d or 2w.
func parseTimeFlag(name, value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	if d, ok := parseAgo(value); ok {
		return time.Now().Add(-d).UnixMilli(), nil
	}
	for _, layout := range timeLayouts {
		if t, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			return t.UnixMilli(), nil
		}
	}
	return 0, fmt.Errorf("%w: --%s %q isn't an ISO 8601 time or a duration like 24h or 7d", errInvalidArgument, name, value)
}

func parseAgo(value string) (time.Duration, bool) {
	if len(value) < 2 {
		return 0, false
	}
	unit := map[byte]time.Duration{'d': 24 * time.Hour, 'w': 7 * 24 * time.Hour}[value[len(value)-1]]
	if unit != 0 {
		n, err := strconv.Atoi(value[:len(value)-1])
		return time.Duration(n) * unit, err == nil && n >= 0
	}
	d, err := time.ParseDuration(value)
	return d, err == nil && d >= 0
}
