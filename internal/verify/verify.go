package verify

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/beeper/matrix-mirror/internal/mxcrypto"
)

type Result struct {
	BackupVersion   id.KeyBackupVersion
	ImportedKeys    int
	FailedKeys      int
	NoBackup        bool
	UnverifiableKey bool
}

// CrossSign unlocks secret storage with the recovery key (or passphrase),
// signs this device with the self-signing key and saves the key backup key
// to the crypto store. The recovery key itself is only held in memory.
func CrossSign(ctx context.Context, c *mxcrypto.Crypto, recoveryKey string) (*Result, *backup.MegolmBackupKey, error) {
	mach := c.Machine
	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("this account has no secret storage set up (set up recovery in another client first): %w", err)
	}
	key, unverifiable, err := unlock(keyID, keyData, recoveryKey)
	if err != nil {
		return nil, nil, err
	}
	result := &Result{UnverifiableKey: unverifiable}
	if err = mach.FetchCrossSigningKeysFromSSSS(ctx, key); err != nil {
		return nil, nil, fmt.Errorf("fetch cross-signing keys from secret storage: %w", err)
	}
	if err = mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return nil, nil, fmt.Errorf("sign this device: %w", err)
	}
	if err = mach.SignOwnMasterKey(ctx); err != nil {
		return nil, nil, fmt.Errorf("sign master key: %w", err)
	}

	rawBackupKey, err := mach.SSSS.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, key)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("No key backup key in secret storage")
		result.NoBackup = true
		return result, nil, nil
	}
	backupKey, err := backup.MegolmBackupKeyFromBytes(rawBackupKey)
	if err != nil {
		return nil, nil, fmt.Errorf("parse key backup key: %w", err)
	}
	version, err := mach.GetAndVerifyLatestKeyBackupVersion(ctx, backupKey)
	if err != nil {
		return nil, nil, fmt.Errorf("check key backup: %w", err)
	} else if version == nil {
		result.NoBackup = true
		return result, nil, nil
	}
	if err = c.SaveBackupKey(ctx, backupKey); err != nil {
		return nil, nil, fmt.Errorf("save key backup key: %w", err)
	}
	result.BackupVersion = version.Version
	return result, backupKey, nil
}

func unlock(keyID string, keyData *ssss.KeyMetadata, input string) (*ssss.Key, bool, error) {
	input = strings.TrimSpace(input)
	key, err := keyData.VerifyRecoveryKey(keyID, input)
	if errors.Is(err, ssss.ErrInvalidRecoveryKey) && keyData.Passphrase != nil {
		key, err = keyData.VerifyPassphrase(keyID, input)
	}
	switch {
	case errors.Is(err, ssss.ErrUnverifiableKey):
		return key, true, nil
	case errors.Is(err, ssss.ErrInvalidRecoveryKey), errors.Is(err, ssss.ErrIncorrectSSSSKey):
		return nil, false, errors.New("that recovery key is wrong for this account")
	case err != nil:
		return nil, false, err
	}
	return key, false, nil
}

func RestoreBackup(ctx context.Context, c *mxcrypto.Crypto, version id.KeyBackupVersion, backupKey *backup.MegolmBackupKey, progress func(done, total int)) (imported, failed int, err error) {
	keys, err := c.Machine.Client.GetKeyBackup(ctx, version)
	if err != nil {
		return 0, 0, fmt.Errorf("download key backup: %w", err)
	}
	total := 0
	for _, room := range keys.Rooms {
		total += len(room.Sessions)
	}
	log := zerolog.Ctx(ctx)
	for roomID, room := range keys.Rooms {
		for sessionID, data := range room.Sessions {
			sessionData, err := data.SessionData.Decrypt(backupKey)
			if err == nil {
				_, err = c.Machine.ImportRoomKeyFromBackup(ctx, version, roomID, sessionID, sessionData)
			}
			if err != nil {
				log.Debug().Err(err).Stringer("room_id", roomID).Stringer("session_id", sessionID).Msg("Skipping backup key")
				failed++
			} else {
				imported++
			}
			if progress != nil {
				progress(imported+failed, total)
			}
		}
	}
	return imported, failed, nil
}
