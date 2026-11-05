package service

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
)

var testOptions = Options{
	Executable: "/Users/me/go/bin/matrix-mirror",
	Args:       []string{"sync", "--home", "/Users/me/odd & <dir> $HOME 50%", "--backfill-days", "90"},
	LogFile:    "/Users/me/odd & <dir>/logs/launchd.log",
}

func TestRenderPlist(t *testing.T) {
	plutil, err := exec.LookPath("plutil")
	if err != nil {
		t.Skip("plutil not available")
	}
	path := filepath.Join(t.TempDir(), "agent.plist")
	if err = os.WriteFile(path, []byte(renderPlist(testOptions)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", path).CombinedOutput()
	if err != nil {
		t.Fatalf("invalid plist: %v\n%s", err, out)
	}
	var agent struct {
		Label             string
		ProgramArguments  []string
		RunAtLoad         bool
		KeepAlive         bool
		StandardErrorPath string
	}
	if err = json.Unmarshal(out, &agent); err != nil {
		t.Fatal(err)
	}
	wantArgs := append([]string{testOptions.Executable}, testOptions.Args...)
	if agent.Label != launchdLabel || !slices.Equal(agent.ProgramArguments, wantArgs) || !agent.RunAtLoad || !agent.KeepAlive || agent.StandardErrorPath != testOptions.LogFile {
		t.Fatalf("unexpected agent: %+v", agent)
	}
}
