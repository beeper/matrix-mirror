package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/beeper/matrix-mirror/internal/lock"
	"github.com/beeper/matrix-mirror/internal/mxcrypto"
	"github.com/beeper/matrix-mirror/internal/service"
	"github.com/beeper/matrix-mirror/internal/store"
	"github.com/beeper/matrix-mirror/internal/syncer"
)

const defaultBackfillDays = 90

func (a *app) syncCommand() *cobra.Command {
	cfg := syncer.Config{}
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Run the syncer in the foreground",
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
			log := a.syncLogger()
			ctx, cancel := signalContext()
			defer cancel()
			ctx = log.WithContext(ctx)

			cli, err := sess.Client()
			if err != nil {
				return err
			}
			cli.Log = log
			pickleKey, err := sess.PickleKeyBytes()
			if err != nil {
				return err
			}
			c, err := mxcrypto.Open(ctx, a.dir.CryptoDB(), cli, pickleKey, log)
			if err != nil {
				return err
			}
			defer c.Close()
			w, err := store.OpenWriter(ctx, a.dir.MirrorDB(), sess.UserID)
			if err != nil {
				return err
			}
			defer w.Close()

			log.Info().
				Stringer("user_id", sess.UserID).
				Stringer("device_id", sess.DeviceID).
				Int("backfill_days", cfg.BackfillDays).
				Msg("Starting sync")
			err = syncer.New(cli, c, w, log, cfg).Run(ctx)
			if err != nil {
				log.Error().Err(err).Msg("Sync stopped")
				return err
			}
			log.Info().Msg("Sync stopped")
			return nil
		},
	}
	cmd.Flags().IntVar(&cfg.BackfillDays, "backfill-days", defaultBackfillDays, "how many days of history to backfill (0 disables backfill)")
	cmd.Flags().IntVar(&cfg.BackfillWorkers, "backfill-workers", 3, "rooms to backfill in parallel")
	cmd.Flags().DurationVar(&cfg.KeyRetryInterval, "key-retry-interval", 5*time.Minute, "how often to look for missing room keys")
	_ = cmd.Flags().MarkHidden("backfill-workers")
	_ = cmd.Flags().MarkHidden("key-retry-interval")
	return cmd
}

func (a *app) installCommand() *cobra.Command {
	var backfillDays int
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Run `sync` as a background service (launchd on macOS, systemd on Linux)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sess, err := a.loadSession()
			if err != nil {
				return err
			}
			if lock.Held(a.dir.Lock()) && !service.Installed() {
				return errors.New("sync is already running in the foreground: stop it first")
			}
			exe, err := stableExecutable()
			if err != nil {
				return err
			}
			if err = a.dir.Ensure(); err != nil {
				return err
			}
			installed, err := service.Install(service.Options{
				Executable: exe,
				Args:       []string{"sync", "--home", a.dir.Root, "--backfill-days", strconv.Itoa(backfillDays)},
				LogFile:    filepath.Join(a.dir.Logs(), "launchd.log"),
			})
			if err != nil {
				return err
			}
			t := newTerminal()
			t.Say("Installed %s %s, running %s.", service.Kind, installed.Path, exe)
			t.Say("Sync is running for %s. Re-run `matrix-mirror install` after upgrading the binary to restart onto it.", sess.UserID)
			for _, note := range installed.Notes {
				t.Say("%s", note)
			}
			if sess.VerifiedAt == nil {
				t.Say("This device isn't verified yet, so history won't decrypt: run `matrix-mirror uninstall`, `matrix-mirror verify`, then install again.")
			}
			return nil
		},
	}
	cmd.Flags().IntVar(&backfillDays, "backfill-days", defaultBackfillDays, "passed through to `sync`")
	return cmd
}

func (a *app) uninstallCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop and remove the background service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			removed, err := service.Uninstall()
			if err != nil {
				return err
			}
			if removed {
				newTerminal().Say("Stopped sync and removed the %s.", service.Kind)
			} else {
				newTerminal().Say("The %s wasn't installed.", service.Kind)
			}
			return nil
		},
	}
}

// stableExecutable refuses `go run` binaries, which launchd couldn't start later.
func stableExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return "", err
	}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return "", errors.New("install from a built binary (`go install`), not `go run`")
	}
	return exe, nil
}
