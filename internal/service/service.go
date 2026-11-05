package service

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
)

var ErrUnsupported = errors.New("installing a background service isn't supported on " + runtime.GOOS + ": run `matrix-mirror sync` under your own supervisor")

type Options struct {
	Executable string
	Args       []string
	LogFile    string
}

type Installation struct {
	Path  string
	Notes []string
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
