//go:build e2e

// End-to-end test against a real homeserver: scripts/e2e.sh starts a
// throwaway Synapse and runs this with MM_E2E_HOMESERVER and MM_E2E_SECRET.
package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/cryptohelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/synapseadmin"

	"github.com/beeper/matrix-mirror/internal/sqlitedb"
)

const password = "correct horse battery staple"

type device struct {
	cli  *mautrix.Client
	mach *crypto.OlmMachine
}

func newDevice(t *testing.T, ctx context.Context, hs, user, name string) *device {
	t.Helper()
	cli, err := mautrix.NewClient(hs, "", "")
	must(t, err)
	raw, err := sqlitedb.Open(filepath.Join(t.TempDir(), name+".db"), false)
	must(t, err)
	db, err := dbutil.NewWithDB(raw, "sqlite")
	must(t, err)
	helper, err := cryptohelper.NewCryptoHelper(cli, []byte("pickle-"+name), db)
	must(t, err)
	helper.LoginAs = &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: user},
		Password:                 password,
		InitialDeviceDisplayName: name,
	}
	must(t, helper.Init(ctx))
	cli.Crypto = helper
	syncCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = cli.SyncWithContext(syncCtx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = helper.Close()
	})
	return &device{cli: cli, mach: helper.Machine()}
}

func register(t *testing.T, ctx context.Context, hs, secret, username string) id.UserID {
	t.Helper()
	cli, err := mautrix.NewClient(hs, "", "")
	must(t, err)
	resp, err := (&synapseadmin.Client{Client: cli}).SharedSecretRegister(ctx, secret, synapseadmin.ReqSharedSecretRegister{
		Username: username, Password: password, InhibitLogin: true,
	})
	must(t, err)
	return resp.UserID
}

type mirror struct {
	t    *testing.T
	bin  string
	home string
	sync *exec.Cmd
}

func (m *mirror) run(stdin string, args ...string) (string, error) {
	cmd := exec.Command(m.bin, append([]string{"--home", m.home}, args...)...)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w\nstdout: %s\nstderr: %s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String() + stderr.String(), nil
}

func (m *mirror) json(out any, args ...string) error {
	stdout, err := m.run("", append(args, "--json")...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(stdout), out)
}

func (m *mirror) startSync() {
	logFile, err := os.OpenFile(filepath.Join(m.home, "sync-output.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	must(m.t, err)
	m.sync = exec.Command(m.bin, "--home", m.home, "--log-level", "debug", "sync", "--backfill-days", "30", "--key-retry-interval", "2s")
	m.sync.Stdout, m.sync.Stderr = logFile, logFile
	must(m.t, m.sync.Start())
}

func (m *mirror) stopSync() {
	if m.sync == nil {
		return
	}
	_ = m.sync.Process.Signal(syscall.SIGTERM)
	_ = m.sync.Wait()
	m.sync = nil
}

func (m *mirror) dumpLogs() {
	data, _ := os.ReadFile(filepath.Join(m.home, "logs", "sync.log"))
	lines := strings.Split(string(data), "\n")
	if len(lines) > 400 {
		lines = lines[len(lines)-400:]
	}
	m.t.Logf("sync.log tail:\n%s", strings.Join(lines, "\n"))
	output, _ := os.ReadFile(filepath.Join(m.home, "sync-output.log"))
	m.t.Logf("sync output:\n%s", output)
}

type jsonMessage struct {
	EventID       id.EventID `json:"event_id"`
	Sender        id.UserID  `json:"sender"`
	SenderName    *string    `json:"sender_name"`
	FromMe        bool       `json:"from_me"`
	Body          *string    `json:"body"`
	MsgType       *string    `json:"msgtype"`
	EditedAt      *int64     `json:"edited_at"`
	Redacted      bool       `json:"redacted"`
	DecryptStatus string     `json:"decrypt_status"`
	ReplyTo       *struct {
		EventID id.EventID `json:"event_id"`
		Body    *string    `json:"body"`
	} `json:"reply_to"`
}

type readOutput struct {
	V    int `json:"v"`
	Chat struct {
		RoomID id.RoomID `json:"room_id"`
		Name   string    `json:"name"`
		Unread int       `json:"unread"`
	} `json:"chat"`
	Messages []jsonMessage `json:"messages"`
}

func (m *mirror) messages(roomID id.RoomID) (map[id.EventID]jsonMessage, error) {
	var out readOutput
	if err := m.json(&out, "read", roomID.String(), "--limit", "0"); err != nil {
		return nil, err
	}
	byID := make(map[id.EventID]jsonMessage, len(out.Messages))
	for _, msg := range out.Messages {
		byID[msg.EventID] = msg
	}
	return byID, nil
}

func expectBody(msgs map[id.EventID]jsonMessage, eventID id.EventID, body string) error {
	msg, ok := msgs[eventID]
	if !ok {
		return fmt.Errorf("%s not mirrored yet", eventID)
	}
	if msg.DecryptStatus == "undecryptable" {
		return fmt.Errorf("%s still undecryptable", eventID)
	}
	if msg.Body == nil || *msg.Body != body {
		return fmt.Errorf("%s body is %v, want %q", eventID, msg.Body, body)
	}
	return nil
}

func eventually(t *testing.T, timeout time.Duration, what string, check func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := check()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %v", what, err)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func send(t *testing.T, ctx context.Context, d *device, roomID id.RoomID, content any) id.EventID {
	t.Helper()
	resp, err := d.cli.SendMessageEvent(ctx, roomID, event.EventMessage, content)
	must(t, err)
	return resp.EventID
}

func text(body string) *event.MessageEventContent {
	return &event.MessageEventContent{MsgType: event.MsgText, Body: body}
}

func sessionOf(t *testing.T, ctx context.Context, d *device, roomID id.RoomID, eventID id.EventID) id.SessionID {
	t.Helper()
	evt, err := d.cli.GetEvent(ctx, roomID, eventID)
	must(t, err)
	must(t, evt.Content.ParseRaw(event.EventEncrypted))
	return evt.Content.AsEncrypted().SessionID
}

func waitForSession(t *testing.T, ctx context.Context, d *device, roomID id.RoomID, sessionID id.SessionID) {
	t.Helper()
	eventually(t, 30*time.Second, "alice's primary device receiving "+sessionID.String(), func() error {
		sess, err := d.mach.CryptoStore.GetGroupSession(ctx, roomID, sessionID)
		if err != nil || sess == nil {
			return fmt.Errorf("not yet: %v", err)
		}
		return nil
	})
}

func uploadToBackup(t *testing.T, ctx context.Context, d *device, version id.KeyBackupVersion, key *backup.MegolmBackupKey, roomID id.RoomID, sessionID id.SessionID) {
	t.Helper()
	igs, err := d.mach.CryptoStore.GetGroupSession(ctx, roomID, sessionID)
	must(t, err)
	firstIndex := igs.Internal.FirstKnownIndex()
	exported, err := igs.Internal.Export(firstIndex)
	must(t, err)
	encrypted, err := backup.EncryptSessionData(key, &backup.MegolmSessionData{
		Algorithm:         id.AlgorithmMegolmV1,
		SenderKey:         igs.SenderKey,
		SenderClaimedKeys: backup.SenderClaimedKeys{Ed25519: igs.SigningKey},
		SessionKey:        string(exported),
	})
	must(t, err)
	raw, err := json.Marshal(encrypted)
	must(t, err)
	_, err = d.cli.PutKeysInBackup(ctx, version, &mautrix.ReqKeyBackup{Rooms: map[id.RoomID]mautrix.ReqRoomKeyBackup{
		roomID: {Sessions: map[id.SessionID]mautrix.ReqKeyBackupData{sessionID: {FirstMessageIndex: int(firstIndex), SessionData: raw}}},
	}})
	must(t, err)
}

func TestMirror(t *testing.T) {
	hs, secret := os.Getenv("MM_E2E_HOMESERVER"), os.Getenv("MM_E2E_SECRET")
	if hs == "" || secret == "" {
		t.Skip("run through scripts/e2e.sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m := &mirror{t: t, bin: filepath.Join(t.TempDir(), "matrix-mirror"), home: filepath.Join(t.TempDir(), "home")}
	build := exec.Command("go", "build", "-tags", "goolm", "-o", m.bin, "../cmd/matrix-mirror")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if os.Getenv("MM_E2E_RACE") != "" {
		build = exec.Command("go", "build", "-race", "-tags", "goolm", "-o", m.bin, "../cmd/matrix-mirror")
	}
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	defer m.stopSync()
	defer func() {
		if t.Failed() || os.Getenv("MM_E2E_LOGS") != "" {
			m.dumpLogs()
		}
	}()

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)
	aliceID := register(t, ctx, hs, secret, "alice"+suffix)
	bobID := register(t, ctx, hs, secret, "bob"+suffix)
	alice := newDevice(t, ctx, hs, aliceID.Localpart(), "alice-primary")
	bob := newDevice(t, ctx, hs, bobID.Localpart(), "bob-phone")
	bob.mach.DisableDeviceChangeKeyRotation = true

	t.Log("Setting up alice's cross-signing, secret storage and key backup")
	recoveryKey, _, err := alice.mach.GenerateAndUploadCrossSigningKeysWithPassword(ctx, password, "")
	must(t, err)
	must(t, alice.mach.SignOwnDevice(ctx, alice.mach.OwnIdentity()))
	must(t, alice.mach.SignOwnMasterKey(ctx))
	keyID, keyData, err := alice.mach.SSSS.GetDefaultKeyData(ctx)
	must(t, err)
	ssssKey, err := keyData.VerifyRecoveryKey(keyID, recoveryKey)
	must(t, err)
	backupKey, err := backup.NewMegolmBackupKey()
	must(t, err)
	created, err := alice.cli.CreateKeyBackupVersion(ctx, &mautrix.ReqRoomKeysVersionCreate[backup.MegolmAuthData]{
		Algorithm: id.KeyBackupAlgorithmMegolmBackupV1,
		AuthData:  backup.MegolmAuthData{PublicKey: id.Ed25519(base64.RawStdEncoding.EncodeToString(backupKey.PublicKey().Bytes()))},
	})
	must(t, err)
	backupVersion := created.Version
	must(t, alice.mach.SSSS.SetEncryptedAccountData(ctx, event.AccountDataMegolmBackupKey, backupKey.Bytes(), ssssKey))

	t.Log("Creating an encrypted chat with history")
	stateKey := ""
	createResp, err := bob.cli.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name:   "E2E Chat",
		Preset: "private_chat",
		Invite: []id.UserID{aliceID},
		InitialState: []*event.Event{{
			Type:     event.StateEncryption,
			StateKey: &stateKey,
			Content:  event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}},
		}},
	})
	must(t, err)
	roomID := createResp.RoomID
	_, err = alice.cli.JoinRoomByID(ctx, roomID)
	must(t, err)
	eventually(t, 30*time.Second, "bob seeing alice join", func() error {
		member, err := bob.cli.StateStore.GetMember(ctx, roomID, aliceID)
		if err != nil || member.Membership != event.MembershipJoin {
			return fmt.Errorf("not joined yet: %v", err)
		}
		return nil
	})

	historyOne := send(t, ctx, bob, roomID, text("history one"))
	historyTwo := send(t, ctx, bob, roomID, text("history two"))
	for i := range 25 {
		send(t, ctx, bob, roomID, text(fmt.Sprintf("filler %d", i)))
	}
	backedUpSession := sessionOf(t, ctx, bob, roomID, historyOne)
	waitForSession(t, ctx, alice, roomID, backedUpSession)
	uploadToBackup(t, ctx, alice, backupVersion, backupKey, roomID, backedUpSession)

	must(t, bob.mach.CryptoStore.RemoveOutboundGroupSession(ctx, roomID))
	lateBackup := send(t, ctx, bob, roomID, text("key reaches the backup later"))
	lateBackupSession := sessionOf(t, ctx, bob, roomID, lateBackup)
	waitForSession(t, ctx, alice, roomID, lateBackupSession)

	must(t, bob.mach.CryptoStore.RemoveOutboundGroupSession(ctx, roomID))
	unsharedSession := sessionOf(t, ctx, bob, roomID, send(t, ctx, bob, roomID, text("before the mirror existed")))
	waitForSession(t, ctx, alice, roomID, unsharedSession)

	t.Log("Logging the mirror in and verifying it")
	out, err := m.run(password+"\n", "login", "--homeserver", hs, "--user", aliceID.Localpart(), "--device-name", "matrix-mirror e2e")
	must(t, err)
	t.Log(strings.TrimSpace(out))
	out, err = m.run("", "verify")
	if err == nil {
		t.Fatal("verify should fail without a recovery key")
	}
	out, err = m.run("not a recovery key\n", "verify")
	if err == nil || !strings.Contains(err.Error(), "recovery key is wrong") {
		t.Fatalf("wrong recovery key should be rejected: %v", err)
	}
	out, err = m.run(recoveryKey+"\n", "verify")
	must(t, err)
	t.Log(strings.TrimSpace(out))
	if !strings.Contains(out, "Restored 1 room keys") {
		t.Fatalf("expected one key restored from backup:\n%s", out)
	}

	var status struct {
		DeviceID id.DeviceID `json:"device_id"`
		Verified bool        `json:"verified"`
	}
	must(t, m.json(&status, "status"))
	if !status.Verified {
		t.Fatal("status doesn't show the device as verified")
	}
	mirrorDevice := status.DeviceID
	eventually(t, 30*time.Second, "alice's primary device trusting the mirror", func() error {
		alice.mach.FetchKeys(ctx, []id.UserID{aliceID}, true)
		device, err := alice.mach.GetOrFetchDevice(ctx, aliceID, mirrorDevice)
		if err != nil {
			return err
		}
		if trust, err := alice.mach.ResolveTrustContext(ctx, device); err != nil || trust < id.TrustStateCrossSignedTOFU {
			return fmt.Errorf("trust is %s (%v)", trust, err)
		}
		return nil
	})

	t.Log("Starting sync")
	m.startSync()
	eventually(t, 60*time.Second, "history decrypting via sync, backfill and the key backup", func() error {
		msgs, err := m.messages(roomID)
		if err != nil {
			return err
		}
		if len(msgs) < 29 {
			return fmt.Errorf("only %d messages mirrored", len(msgs))
		}
		if err = expectBody(msgs, historyOne, "history one"); err != nil {
			return err
		}
		return expectBody(msgs, historyTwo, "history two")
	})
	msgs, err := m.messages(roomID)
	must(t, err)
	if msgs[lateBackup].DecryptStatus != "undecryptable" {
		t.Fatalf("message whose key isn't backed up yet should be undecryptable: %+v", msgs[lateBackup])
	}

	t.Log("Uploading the late key to the backup")
	uploadToBackup(t, ctx, alice, backupVersion, backupKey, roomID, lateBackupSession)

	t.Log("Sending with a session the mirror was never given, so it has to request the key")
	unshared := send(t, ctx, bob, roomID, text("only alice's other device can share this key"))
	if sessionOf(t, ctx, bob, roomID, unshared) != unsharedSession {
		t.Fatal("bob rotated the session; the key request path isn't being tested")
	}
	eventually(t, 60*time.Second, "key request answered by alice's primary device, and the late backup key", func() error {
		msgs, err := m.messages(roomID)
		if err != nil {
			return err
		}
		if err = expectBody(msgs, unshared, "only alice's other device can share this key"); err != nil {
			return err
		}
		return expectBody(msgs, lateBackup, "key reaches the backup later")
	})

	t.Log("Sending live messages in a new session, which bob shares with the mirror directly")
	must(t, bob.mach.CryptoStore.RemoveOutboundGroupSession(ctx, roomID))
	live := send(t, ctx, bob, roomID, text("live one"))
	if sessionOf(t, ctx, bob, roomID, live) == unsharedSession {
		t.Fatal("bob didn't rotate the session")
	}
	send(t, ctx, bob, roomID, &event.MessageEventContent{
		MsgType:    event.MsgText,
		Body:       "* live one (edited)",
		NewContent: &event.MessageEventContent{MsgType: event.MsgText, Body: "live one (edited)"},
		RelatesTo:  &event.RelatesTo{Type: event.RelReplace, EventID: live},
	})
	doomed := send(t, ctx, bob, roomID, text("this gets deleted"))
	_, err = bob.cli.RedactEvent(ctx, roomID, doomed)
	must(t, err)
	reply := send(t, ctx, bob, roomID, &event.MessageEventContent{
		MsgType:   event.MsgText,
		Body:      "replying to history",
		RelatesTo: &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: historyTwo}},
	})

	eventually(t, 30*time.Second, "live messages, edits and redactions", func() error {
		msgs, err := m.messages(roomID)
		if err != nil {
			return err
		}
		if err = expectBody(msgs, live, "live one (edited)"); err != nil {
			return err
		}
		if msgs[live].EditedAt == nil {
			return fmt.Errorf("edit not marked")
		}
		if d := msgs[doomed]; !d.Redacted || d.Body != nil {
			return fmt.Errorf("redaction not applied: %+v", d)
		}
		if r := msgs[reply]; r.ReplyTo == nil || r.ReplyTo.EventID != historyTwo || r.ReplyTo.Body == nil || *r.ReplyTo.Body != "history two" {
			return fmt.Errorf("reply context wrong: %+v", r.ReplyTo)
		}
		return nil
	})
	var keyStatus struct {
		MissingKeys   int `json:"missing_keys"`
		Undecryptable int `json:"undecryptable"`
	}
	must(t, m.json(&keyStatus, "status"))
	if keyStatus.MissingKeys != 0 || keyStatus.Undecryptable != 0 {
		t.Fatalf("live messages should decrypt with keys bob shared: %+v", keyStatus)
	}

	t.Log("Checking a plain room joined while syncing gets its history backfilled")
	plainResp, err := bob.cli.CreateRoom(ctx, &mautrix.ReqCreateRoom{Name: "Plain Chat", Preset: "private_chat", Invite: []id.UserID{aliceID}})
	must(t, err)
	plainRoom := plainResp.RoomID
	for i := range 30 {
		send(t, ctx, bob, plainRoom, text(fmt.Sprintf("plain before join %d", i)))
	}
	_, err = alice.cli.JoinRoomByID(ctx, plainRoom)
	must(t, err)
	eventually(t, 60*time.Second, "plain room backfilled", func() error {
		msgs, err := m.messages(plainRoom)
		if err != nil {
			return err
		}
		if len(msgs) < 30 {
			return fmt.Errorf("%d of 30 plain messages", len(msgs))
		}
		for _, msg := range msgs {
			if msg.DecryptStatus != "plaintext" {
				return fmt.Errorf("plain message has status %s", msg.DecryptStatus)
			}
		}
		return nil
	})

	t.Log("Checking chats, search and unread")
	var chats struct {
		V     int `json:"v"`
		Chats []struct {
			RoomID  id.RoomID `json:"room_id"`
			Name    string    `json:"name"`
			Network *string   `json:"network"`
			Unread  int       `json:"unread"`
		} `json:"chats"`
	}
	must(t, m.json(&chats, "chats"))
	if chats.V != 1 || len(chats.Chats) != 2 || chats.Chats[0].Name != "Plain Chat" || chats.Chats[1].Name != "E2E Chat" || chats.Chats[0].Network != nil {
		t.Fatalf("unexpected chats: %+v", chats)
	}
	var search struct {
		Results []struct {
			EventID  id.EventID `json:"event_id"`
			ChatName string     `json:"chat_name"`
			Snippet  string     `json:"snippet"`
		} `json:"results"`
	}
	must(t, m.json(&search, "search", "history", "--chat", "e2e"))
	if len(search.Results) != 3 || search.Results[0].EventID != reply || search.Results[0].ChatName != "E2E Chat" || !strings.Contains(search.Results[0].Snippet, "[history]") {
		t.Fatalf("unexpected search results: %+v", search)
	}
	must(t, m.json(&search, "search", "deleted"))
	if len(search.Results) != 0 {
		t.Fatalf("redacted message is searchable: %+v", search)
	}
	if _, err = m.run("", "read", "chat"); err == nil || !strings.Contains(err.Error(), "matches 2 chats") {
		t.Fatalf("ambiguous chat name should fail: %v", err)
	}

	var unread struct {
		TotalUnread int `json:"total_unread"`
	}
	must(t, m.json(&unread, "unread"))
	if unread.TotalUnread == 0 {
		t.Fatal("expected unread messages before alice reads anything")
	}
	must(t, alice.cli.SendReceipt(ctx, roomID, reply, event.ReceiptTypeRead, nil))
	eventually(t, 30*time.Second, "read receipt clearing unread", func() error {
		var out readOutput
		if err := m.json(&out, "read", roomID.String(), "--limit", "1"); err != nil {
			return err
		}
		if out.Chat.Unread != 0 {
			return fmt.Errorf("%d unread", out.Chat.Unread)
		}
		return nil
	})

	t.Log("Restarting sync to check it resumes rather than starting over")
	before, err := m.messages(roomID)
	must(t, err)
	m.stopSync()
	m.startSync()
	afterRestart := send(t, ctx, bob, roomID, text("after restart"))
	eventually(t, 60*time.Second, "message after restart", func() error {
		msgs, err := m.messages(roomID)
		if err != nil {
			return err
		}
		if len(msgs) != len(before)+1 {
			return fmt.Errorf("%d messages, expected %d", len(msgs), len(before)+1)
		}
		return expectBody(msgs, afterRestart, "after restart")
	})
	syncLog, err := os.ReadFile(filepath.Join(m.home, "logs", "sync.log"))
	must(t, err)
	for want, count := range map[string]int{"Starting sync": 2, "Initial sync done": 1, `"kind":"initial"`: 1} {
		if got := strings.Count(string(syncLog), want); got < count || (want != `"kind":"initial"` && got != count) {
			t.Fatalf("expected %q %d times in the sync log, found %d", want, count, got)
		}
	}
	if !strings.Contains(string(syncLog), `"message":"Synced"`) {
		t.Fatal("expected live syncs with messages to be logged at info")
	}

	t.Log("Checking file permissions")
	must(t, filepath.Walk(m.home, func(path string, info os.FileInfo, err error) error {
		if err != nil || path == filepath.Join(m.home, "sync-output.log") {
			return err
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			return fmt.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
		}
		return nil
	}))

	m.stopSync()
	out, err = m.run("", "logout")
	must(t, err)
	if _, err = os.Stat(filepath.Join(m.home, "crypto.db")); !os.IsNotExist(err) {
		t.Fatal("crypto.db should be deleted on logout")
	}
	if _, err = m.messages(roomID); err != nil {
		t.Fatalf("mirrored chats should survive logout: %v", err)
	}
}
