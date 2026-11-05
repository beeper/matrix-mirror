package paths

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

const EnvHome = "MATRIX_MIRROR_HOME"

type Dir struct {
	Root string
}

func Default() (string, error) {
	if home := os.Getenv(EnvHome); home != "" {
		return home, nil
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(userHome, ".matrix-mirror"), nil
}

func New(root string) (Dir, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Dir{}, err
	}
	return Dir{Root: abs}, nil
}

func (d Dir) MirrorDB() string { return filepath.Join(d.Root, "mirror.db") }
func (d Dir) CryptoDB() string { return filepath.Join(d.Root, "crypto.db") }
func (d Dir) Secrets() string  { return filepath.Join(d.Root, "secrets") }
func (d Dir) Session() string  { return filepath.Join(d.Secrets(), "session.json") }
func (d Dir) Logs() string     { return filepath.Join(d.Root, "logs") }
func (d Dir) Lock() string     { return filepath.Join(d.Root, "sync.lock") }

// Ensure creates the data directories and tightens anything already in them
// to owner-only permissions: directories 0700, files 0600.
func (d Dir) Ensure() error {
	for _, dir := range []string{d.Root, d.Secrets(), d.Logs()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return filepath.WalkDir(d.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		want := fs.FileMode(0o600)
		if entry.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want && info.Mode().Type()&fs.ModeSymlink == 0 {
			if err = os.Chmod(path, want); err != nil {
				return fmt.Errorf("tighten permissions on %s: %w", path, err)
			}
		}
		return nil
	})
}
