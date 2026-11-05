package service

import (
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
)

const systemdUnit = "matrix-mirror.service"

func unitPath() (string, error) {
	configDir := os.Getenv("XDG_CONFIG_HOME")
	if configDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		configDir = filepath.Join(home, ".config")
	}
	return filepath.Join(configDir, "systemd", "user", systemdUnit), nil
}

func renderUnit(opts Options) string {
	args := make([]string, 0, len(opts.Args)+1)
	for _, arg := range append([]string{opts.Executable}, opts.Args...) {
		args = append(args, systemdQuote(arg))
	}
	return `[Unit]
Description=matrix-mirror sync

[Service]
ExecStart=` + strings.Join(args, " ") + `
Restart=always
RestartSec=30

[Install]
WantedBy=default.target
`
}

// systemdQuote escapes an ExecStart argument: C-style escapes inside double
// quotes, plus systemd's % specifiers and $ variable expansion.
func systemdQuote(arg string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(arg) + `"`
}

func systemdInstalled() bool {
	path, err := unitPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// installSystemd writes a systemd user service, enables it and (re)starts
// it. User services start at login, or at boot when lingering is enabled.
func installSystemd(opts Options) (*Installation, error) {
	path, err := unitPath()
	if err != nil {
		return nil, err
	}
	if err = writeFile(path, renderUnit(opts)); err != nil {
		return nil, err
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", systemdUnit}, {"restart", systemdUnit}} {
		if err = run("systemctl", append([]string{"--user"}, args...)...); err != nil {
			return nil, err
		}
	}
	notes := []string{"Logs: journalctl --user -u " + systemdUnit}
	if current, err := user.Current(); err == nil && !lingering(current.Username) {
		notes = append(notes, "To start it at boot and keep it running after you log out: loginctl enable-linger "+current.Username)
	}
	return &Installation{Path: path, Notes: notes}, nil
}

func lingering(username string) bool {
	out, err := exec.Command("loginctl", "show-user", username, "--property=Linger", "--value").Output()
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

func uninstallSystemd() (bool, error) {
	path, err := unitPath()
	if err != nil {
		return false, err
	}
	_ = run("systemctl", "--user", "disable", "--now", systemdUnit)
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return true, run("systemctl", "--user", "daemon-reload")
}
