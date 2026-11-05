package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const launchdLabel = "com.beeper.matrix-mirror"

func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist"), nil
}

func renderPlist(opts Options) string {
	var args strings.Builder
	for _, arg := range append([]string{opts.Executable}, opts.Args...) {
		args.WriteString("\t\t<string>" + xmlEscape(arg) + "</string>\n")
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array>
` + args.String() + `	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>
	<string>` + xmlEscape(opts.LogFile) + `</string>
	<key>StandardErrorPath</key>
	<string>` + xmlEscape(opts.LogFile) + `</string>
</dict>
</plist>
`
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	_ = xml.EscapeText(&buf, []byte(s))
	return buf.String()
}

func launchdTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

func launchdInstalled() bool {
	path, err := plistPath()
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// installLaunchd writes a LaunchAgent and (re)starts it. LaunchAgents start
// at login, so sync comes back after a reboot once the user logs in.
func installLaunchd(opts Options) (*Installation, error) {
	path, err := plistPath()
	if err != nil {
		return nil, err
	}
	if err = writeFile(path, renderPlist(opts)); err != nil {
		return nil, err
	}
	_ = run("launchctl", "bootout", launchdTarget()+"/"+launchdLabel)
	if err = run("launchctl", "enable", launchdTarget()+"/"+launchdLabel); err != nil {
		return nil, err
	}
	// bootstrap can fail briefly while a booted-out instance is still exiting.
	for attempt := 0; ; attempt++ {
		err = run("launchctl", "bootstrap", launchdTarget(), path)
		if err == nil {
			return &Installation{Path: path, Notes: []string{"Logs: " + filepath.Dir(opts.LogFile)}}, nil
		} else if attempt == 4 {
			return nil, err
		}
		time.Sleep(time.Second)
	}
}

func uninstallLaunchd() (bool, error) {
	path, err := plistPath()
	if err != nil {
		return false, err
	}
	_ = run("launchctl", "bootout", launchdTarget()+"/"+launchdLabel)
	if err = os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}
