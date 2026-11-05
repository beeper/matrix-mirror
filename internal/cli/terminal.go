package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// terminal asks questions on stderr and reads answers from stdin. When stdin
// isn't a terminal, answers are read line by line so they can be piped in.
type terminal struct {
	in    *bufio.Reader
	out   io.Writer
	fd    int
	isTTY bool
}

func newTerminal() *terminal {
	fd := int(os.Stdin.Fd())
	return &terminal{in: bufio.NewReader(os.Stdin), out: os.Stderr, fd: fd, isTTY: term.IsTerminal(fd)}
}

func (t *terminal) Ask(question string) (string, error) {
	_, _ = fmt.Fprint(t.out, question)
	line, err := t.in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", fmt.Errorf("read %s: %w", strings.TrimSuffix(strings.TrimSpace(question), ":"), err)
	}
	return strings.TrimSpace(line), nil
}

func (t *terminal) AskSecret(question string) (string, error) {
	if !t.isTTY {
		return t.Ask(question)
	}
	_, _ = fmt.Fprint(t.out, question)
	secret, err := term.ReadPassword(t.fd)
	_, _ = fmt.Fprintln(t.out)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(secret)), nil
}

func (t *terminal) Say(format string, args ...any) {
	_, _ = fmt.Fprintf(t.out, format+"\n", args...)
}
