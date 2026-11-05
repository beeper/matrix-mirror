package session

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

var ErrNotLoggedIn = errors.New("not logged in: run `matrix-mirror login` first")

type Session struct {
	Homeserver  string      `json:"homeserver"`
	UserID      id.UserID   `json:"user_id"`
	DeviceID    id.DeviceID `json:"device_id"`
	AccessToken string      `json:"access_token"`
	PickleKey   string      `json:"pickle_key"`
	Provider    string      `json:"provider"`
	DeviceName  string      `json:"device_name"`
	CreatedAt   time.Time   `json:"created_at"`
	VerifiedAt  *time.Time  `json:"verified_at,omitempty"`
}

func NewPickleKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(key), nil
}

func Load(path string) (*Session, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotLoggedIn
	} else if err != nil {
		return nil, err
	}
	var s Session
	if err = json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return &s, nil
}

func (s *Session) Save(path string) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".session-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Session) PickleKeyBytes() ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(s.PickleKey)
}

func (s *Session) Client() (*mautrix.Client, error) {
	cli, err := mautrix.NewClient(s.Homeserver, s.UserID, s.AccessToken)
	if err != nil {
		return nil, err
	}
	cli.DeviceID = s.DeviceID
	cli.UserAgent = UserAgent
	return cli, nil
}

var UserAgent = "matrix-mirror"
