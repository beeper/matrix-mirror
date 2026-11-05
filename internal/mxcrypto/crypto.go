package mxcrypto

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/beeper/matrix-mirror/internal/sqlitedb"
)

type Crypto struct {
	DB         *dbutil.Database
	Store      *crypto.SQLCryptoStore
	StateStore *sqlstatestore.SQLStateStore
	Machine    *crypto.OlmMachine
}

func Open(ctx context.Context, path string, cli *mautrix.Client, pickleKey []byte, log zerolog.Logger) (*Crypto, error) {
	raw, err := sqlitedb.Open(path, false)
	if err != nil {
		return nil, err
	}
	db, err := dbutil.NewWithDB(raw, "sqlite")
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	c, err := open(ctx, db, cli, pickleKey, log)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	return c, nil
}

func open(ctx context.Context, db *dbutil.Database, cli *mautrix.Client, pickleKey []byte, log zerolog.Logger) (*Crypto, error) {
	stateStore := sqlstatestore.NewSQLStateStore(db, dbutil.ZeroLogger(log.With().Str("db_section", "state").Logger()), false)
	if err := stateStore.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade crypto state store: %w", err)
	}
	cryptoStore := crypto.NewSQLCryptoStore(db, dbutil.ZeroLogger(log.With().Str("db_section", "crypto").Logger()), "", cli.DeviceID, pickleKey)
	if err := cryptoStore.DB.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("upgrade crypto store: %w", err)
	}
	storedDeviceID, err := cryptoStore.FindDeviceID(ctx)
	if err != nil {
		return nil, err
	}
	if storedDeviceID != "" && storedDeviceID != cli.DeviceID {
		return nil, fmt.Errorf("crypto.db belongs to device %s, but the session is device %s: run `matrix-mirror logout` and log in again", storedDeviceID, cli.DeviceID)
	}
	cli.StateStore = stateStore
	machLog := log.With().Str("component", "crypto").Logger()
	mach := crypto.NewOlmMachine(cli, &machLog, cryptoStore, stateStore)
	if err = mach.Load(ctx); err != nil {
		return nil, fmt.Errorf("load olm account: %w", err)
	}
	if err = ensureDeviceKeys(ctx, cli, mach, log); err != nil {
		return nil, err
	}
	return &Crypto{DB: db, Store: cryptoStore, StateStore: stateStore, Machine: mach}, nil
}

func (c *Crypto) Close() error {
	return c.DB.Close()
}

// ensureDeviceKeys uploads this device's identity keys on first use and
// refuses to continue if the server holds different ones.
func ensureDeviceKeys(ctx context.Context, cli *mautrix.Client, mach *crypto.OlmMachine, log zerolog.Logger) error {
	resp, err := cli.QueryKeys(ctx, &mautrix.ReqQueryKeys{
		DeviceKeys: map[id.UserID]mautrix.DeviceIDList{cli.UserID: {cli.DeviceID}},
	})
	if err != nil {
		return fmt.Errorf("query own device keys: %w", err)
	}
	isShared := mach.GetAccount().Shared
	device, ok := resp.DeviceKeys[cli.UserID][cli.DeviceID]
	switch {
	case !ok || len(device.Keys) == 0:
		if isShared {
			return errors.New("this device's keys have disappeared from the server: run `matrix-mirror logout` and log in again")
		}
		log.Debug().Msg("Uploading initial device keys")
		if err = mach.ShareKeys(ctx, -1); err != nil {
			return fmt.Errorf("upload device keys: %w", err)
		}
	case !isShared:
		return errors.New("the server has keys for this device that crypto.db doesn't: run `matrix-mirror logout` and log in again")
	case device.Keys.GetEd25519(cli.DeviceID) != mach.OwnIdentity().SigningKey:
		return errors.New("the server's identity key for this device doesn't match crypto.db")
	}
	return nil
}

// BackupKey returns the key backup decryption key saved by `verify`, or nil.
func (c *Crypto) BackupKey(ctx context.Context) (*backup.MegolmBackupKey, error) {
	encoded, err := c.Store.GetSecret(ctx, id.SecretMegolmBackupV1)
	if err != nil || encoded == "" {
		return nil, err
	}
	raw, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("decode stored backup key: %w", err)
	}
	return backup.MegolmBackupKeyFromBytes(raw)
}

func (c *Crypto) SaveBackupKey(ctx context.Context, key *backup.MegolmBackupKey) error {
	return c.Store.PutSecret(ctx, id.SecretMegolmBackupV1, base64.RawStdEncoding.EncodeToString(key.Bytes()))
}
