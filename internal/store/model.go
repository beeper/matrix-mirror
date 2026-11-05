package store

import (
	"encoding/json"

	"maunium.net/go/mautrix/id"
)

const (
	StatusOK            = "ok"
	StatusUndecryptable = "undecryptable"
	StatusPlaintext     = "plaintext"

	MembershipJoin  = "join"
	MembershipLeave = "leave"

	BackfillInitial = "initial"
	BackfillGap     = "gap"
)

type Attachment struct {
	Filename string `json:"filename"`
	MimeType string `json:"mimetype"`
	URL      string `json:"url"`
}

type Message struct {
	EventID       id.EventID
	RoomID        id.RoomID
	Sender        id.UserID
	TS            int64
	MsgType       string
	Body          string
	ReplyTo       id.EventID
	Redacted      bool
	DecryptStatus string
	Attachment    *Attachment
}

type Edit struct {
	EventID    id.EventID
	Target     id.EventID
	Sender     id.UserID
	TS         int64
	MsgType    string
	Body       string
	Attachment *Attachment
}

type Redaction struct {
	EventID id.EventID
	Redacts id.EventID
	TS      int64
}

// Undecryptable is an encrypted event we couldn't decrypt yet. Placeholder is
// set when it should show up as a message row before it's decrypted.
type Undecryptable struct {
	Placeholder *Message
	EventID     id.EventID
	RoomID      id.RoomID
	Sender      id.UserID
	SessionID   id.SessionID
	SenderKey   id.SenderKey
	DeviceID    id.DeviceID
	Raw         json.RawMessage
	Live        bool
}

type Member struct {
	RoomID      id.RoomID
	UserID      id.UserID
	DisplayName string
	Membership  string
}

type Receipt struct {
	RoomID  id.RoomID
	EventID id.EventID
	TS      int64
}

type RoomUpdate struct {
	RoomID         id.RoomID
	Membership     string
	ExplicitName   *string
	CanonicalAlias *string
	Heroes         []id.UserID
	RoomType       *string
	Network        *string
	NetworkName    *string
	BridgeBot      *string
	IsDM           bool
	Encrypted      bool
	PrevBatch      string
	Limited        bool
}

type BackfillTask struct {
	ID     int64
	RoomID id.RoomID
	Token  string
	Kind   string
}

type BackfillProgress struct {
	TaskID    int64
	NextToken string
	Done      bool
}

type MissingSession struct {
	SessionID       id.SessionID
	RoomID          id.RoomID
	Sender          id.UserID
	SenderKey       id.SenderKey
	DeviceID        id.DeviceID
	Live            bool
	FirstSeenTS     int64
	BackupCheckedTS int64
	BackupChecks    int
	RequestedTS     int64
}

// Batch is everything one sync response, backfill page or decryption retry
// writes. Writer.Apply writes it in a single transaction.
type Batch struct {
	rooms     map[id.RoomID]*RoomUpdate
	roomOrder []id.RoomID

	Members         []Member
	BackfillMembers []Member
	DirectRooms     []id.RoomID
	Discard         []id.EventID
	Messages        []Message
	Edits           []Edit
	Redactions      []Redaction
	Undecryptable   []Undecryptable
	Resolved        []id.EventID
	Receipts        []Receipt
	Backfill        *BackfillProgress
	SyncState       map[string]string
}

func NewBatch() *Batch {
	return &Batch{rooms: make(map[id.RoomID]*RoomUpdate), SyncState: make(map[string]string)}
}

func (b *Batch) Room(roomID id.RoomID) *RoomUpdate {
	if ru, ok := b.rooms[roomID]; ok {
		return ru
	}
	ru := &RoomUpdate{RoomID: roomID}
	b.rooms[roomID] = ru
	b.roomOrder = append(b.roomOrder, roomID)
	return ru
}

func (b *Batch) Rooms() []*RoomUpdate {
	out := make([]*RoomUpdate, 0, len(b.roomOrder))
	for _, roomID := range b.roomOrder {
		out = append(out, b.rooms[roomID])
	}
	return out
}

func (b *Batch) Empty() bool {
	return len(b.rooms) == 0 && len(b.Members) == 0 && len(b.BackfillMembers) == 0 && len(b.DirectRooms) == 0 &&
		len(b.Discard) == 0 && len(b.Messages) == 0 && len(b.Edits) == 0 && len(b.Redactions) == 0 &&
		len(b.Undecryptable) == 0 && len(b.Resolved) == 0 && len(b.Receipts) == 0 && b.Backfill == nil &&
		len(b.SyncState) == 0
}
