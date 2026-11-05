package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"maunium.net/go/mautrix"

	"github.com/beeper/matrix-mirror/internal/lock"
	"github.com/beeper/matrix-mirror/internal/login"
	"github.com/beeper/matrix-mirror/internal/mxcrypto"
	"github.com/beeper/matrix-mirror/internal/session"
	"github.com/beeper/matrix-mirror/internal/store"
	"github.com/beeper/matrix-mirror/internal/verify"
)

// exclusive prepares the data dir and takes the lock that keeps sync, login,
// logout and verify from touching crypto.db at the same time.
func (a *app) exclusive() (*lock.Lock, error) {
	if err := a.dir.Ensure(); err != nil {
		return nil, err
	}
	return lock.Acquire(a.dir.Lock())
}

func (a *app) loginCommand() *cobra.Command {
	var (
		homeserver   string
		beeper       bool
		beeperDomain string
		email        string
		method       string
		user         string
		token        string
		deviceName   string
	)
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Log in as a new device (--homeserver <url> or --beeper)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			t := newTerminal()
			var provider login.Provider
			switch {
			case beeper && homeserver != "":
				return errors.New("use either --beeper or --homeserver, not both")
			case beeper:
				provider = &login.Beeper{Domain: beeperDomain, Email: email, Terminal: t}
			case homeserver != "":
				provider = &login.Matrix{Server: homeserver, Method: method, User: user, Token: token, Terminal: t}
			default:
				return errors.New("pass --homeserver <url> or --beeper")
			}
			l, err := a.exclusive()
			if err != nil {
				return err
			}
			defer l.Release()
			if existing, err := a.loadSession(); err == nil {
				return fmt.Errorf("already logged in as %s (device %s): run `matrix-mirror logout` first", existing.UserID, existing.DeviceID)
			} else if !errors.Is(err, session.ErrNotLoggedIn) {
				return err
			}
			if err = removeDB(a.dir.CryptoDB()); err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()

			sess, err := login.Login(ctx, provider, deviceName)
			if err != nil {
				return err
			}
			if err = sess.Save(a.dir.Session()); err != nil {
				return fmt.Errorf("save session: %w", err)
			}
			if err = a.resetSyncPosition(ctx, sess); err != nil {
				return err
			}
			c, _, err := a.openCrypto(ctx, sess)
			if err != nil {
				return fmt.Errorf("logged in, but setting up encryption failed (it will be retried): %w", err)
			}
			_ = c.Close()
			t.Say("Logged in as %s on new device %s (%q).", sess.UserID, sess.DeviceID, sess.DeviceName)
			t.Say("Next, run `matrix-mirror verify` with your recovery key.")
			return nil
		},
	}
	cmd.Flags().StringVar(&homeserver, "homeserver", "", "homeserver URL or server name")
	cmd.Flags().BoolVar(&beeper, "beeper", false, "log in to Beeper with an email code")
	cmd.Flags().StringVar(&beeperDomain, "beeper-domain", login.DefaultBeeperDomain, "Beeper environment domain")
	cmd.Flags().StringVar(&email, "email", "", "Beeper account email (prompted if empty)")
	cmd.Flags().StringVar(&method, "method", "", "login method: password, sso or token (default: picked from the server's flows)")
	cmd.Flags().StringVar(&user, "user", "", "username for password login (prompted if empty)")
	cmd.Flags().StringVar(&token, "token", "", "login token (m.login.token)")
	cmd.Flags().StringVar(&deviceName, "device-name", login.DefaultDeviceName(), "display name for the new device")
	_ = cmd.Flags().MarkHidden("beeper-domain")
	return cmd
}

func (a *app) logoutCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Log out this device and delete its keys (the mirrored chats are kept)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.exclusive()
			if err != nil {
				return err
			}
			defer l.Release()
			sess, err := a.loadSession()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			cli, err := sess.Client()
			if err != nil {
				return err
			}
			if _, err = cli.Logout(ctx); err != nil && !errors.Is(err, mautrix.MUnknownToken) {
				return fmt.Errorf("log out on the server: %w", err)
			}
			if err = os.Remove(a.dir.Session()); err != nil {
				return err
			}
			if err = removeDB(a.dir.CryptoDB()); err != nil {
				return err
			}
			if err = a.resetSyncPosition(ctx, sess); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "Logged out device %s. Chats in %s are kept.\n", sess.DeviceID, a.dir.MirrorDB())
			return nil
		},
	}
}

func (a *app) verifyCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "verify",
		Short: "Cross-sign this device with your recovery key and restore the key backup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			l, err := a.exclusive()
			if err != nil {
				return err
			}
			defer l.Release()
			sess, err := a.loadSession()
			if err != nil {
				return err
			}
			ctx, cancel := signalContext()
			defer cancel()
			c, log, err := a.openCrypto(ctx, sess)
			if err != nil {
				return err
			}
			defer c.Close()
			ctx = log.WithContext(ctx)

			t := newTerminal()
			recoveryKey, err := t.AskSecret("Recovery key: ")
			if err != nil {
				return err
			}
			result, backupKey, err := verify.CrossSign(ctx, c, recoveryKey)
			if err != nil {
				return err
			}
			if result.UnverifiableKey {
				t.Say("Warning: secret storage has no checksum, so the recovery key couldn't be checked up front.")
			}
			t.Say("Device %s is now cross-signed; your other clients will show it as verified.", sess.DeviceID)

			now := time.Now().UTC()
			sess.VerifiedAt = &now
			if err = sess.Save(a.dir.Session()); err != nil {
				return err
			}
			if backupKey == nil {
				t.Say("No key backup found, so only new messages will decrypt.")
				return nil
			}
			t.Say("Restoring room keys from key backup version %s...", result.BackupVersion)
			lastPrint := time.Now()
			imported, failed, err := verify.RestoreBackup(ctx, c, result.BackupVersion, backupKey, func(done, total int) {
				if time.Since(lastPrint) > 2*time.Second || done == total {
					lastPrint = time.Now()
					t.Say("  %d/%d", done, total)
				}
			})
			if err != nil {
				return err
			}
			if failed > 0 {
				t.Say("Restored %d room keys (%d couldn't be imported).", imported, failed)
			} else {
				t.Say("Restored %d room keys.", imported)
			}
			return nil
		},
	}
}

func (a *app) openCrypto(ctx context.Context, sess *session.Session) (*mxcrypto.Crypto, zerolog.Logger, error) {
	log := a.consoleLogger()
	cli, err := sess.Client()
	if err != nil {
		return nil, log, err
	}
	cli.Log = log
	pickleKey, err := sess.PickleKeyBytes()
	if err != nil {
		return nil, log, err
	}
	c, err := mxcrypto.Open(ctx, a.dir.CryptoDB(), cli, pickleKey, log)
	return c, log, err
}

// resetSyncPosition makes the next sync an initial one, as a new device must.
func (a *app) resetSyncPosition(ctx context.Context, sess *session.Session) error {
	if _, err := os.Stat(a.dir.MirrorDB()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	w, err := store.OpenWriter(ctx, a.dir.MirrorDB(), sess.UserID)
	if err != nil {
		return err
	}
	defer w.Close()
	return w.DeleteSyncState(ctx, "next_batch", "filter_id", "filter_version")
}

func removeDB(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
