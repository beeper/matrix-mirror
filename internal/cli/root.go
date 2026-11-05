package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/natefinch/lumberjack.v2"

	"github.com/beeper/matrix-mirror/internal/paths"
	"github.com/beeper/matrix-mirror/internal/session"
)

func version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

type app struct {
	home     string
	jsonOut  bool
	logLevel string
	dir      paths.Dir
}

func Execute() int {
	syscall.Umask(0o077)
	a := &app{}
	root := a.rootCommand()
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	if a.jsonOut && isReadCommand(cmd) {
		writeJSONError(os.Stdout, err)
	} else {
		fmt.Fprintln(os.Stderr, "Error:", err)
	}
	return 1
}

func (a *app) rootCommand() *cobra.Command {
	defaultHome, _ := paths.Default()
	root := &cobra.Command{
		Use:           "matrix-mirror",
		Short:         "Keep a local, searchable copy of a Matrix account's chats",
		Version:       version(),
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			session.UserAgent = "matrix-mirror/" + cmd.Root().Version
			dir, err := paths.New(a.home)
			a.dir = dir
			return err
		},
	}
	root.PersistentFlags().StringVar(&a.home, "home", defaultHome, "data directory (env "+paths.EnvHome+")")
	root.PersistentFlags().StringVar(&a.logLevel, "log-level", "", "log level: debug, info, warn, error (default info for sync, warn otherwise)")
	root.AddCommand(
		a.loginCommand(),
		a.logoutCommand(),
		a.verifyCommand(),
		a.syncCommand(),
		a.installCommand(),
		a.uninstallCommand(),
		a.statusCommand(),
		a.chatsCommand(),
		a.readCommand(),
		a.searchCommand(),
		a.unreadCommand(),
	)
	return root
}

const readAnnotation = "read"

func isReadCommand(cmd *cobra.Command) bool {
	return cmd != nil && cmd.Annotations[readAnnotation] == "true"
}

func (a *app) addJSONFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&a.jsonOut, "json", false, "output JSON")
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations[readAnnotation] = "true"
}

func (a *app) level(fallback zerolog.Level) zerolog.Level {
	if a.logLevel == "" {
		return fallback
	}
	level, err := zerolog.ParseLevel(a.logLevel)
	if err != nil {
		return fallback
	}
	return level
}

func (a *app) consoleLogger() zerolog.Logger {
	return zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"}).
		Level(a.level(zerolog.WarnLevel)).With().Timestamp().Logger()
}

// syncLogger writes JSON logs to logs/sync.log, and also to the terminal when
// run in the foreground.
func (a *app) syncLogger() zerolog.Logger {
	writers := []io.Writer{&lumberjack.Logger{
		Filename:   filepath.Join(a.dir.Logs(), "sync.log"),
		MaxSize:    20,
		MaxBackups: 5,
	}}
	if term.IsTerminal(int(os.Stderr.Fd())) {
		writers = append(writers, zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: "15:04:05"})
	}
	return zerolog.New(zerolog.MultiLevelWriter(writers...)).
		Level(a.level(zerolog.InfoLevel)).With().Timestamp().Logger()
}

func (a *app) loadSession() (*session.Session, error) {
	return session.Load(a.dir.Session())
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}
