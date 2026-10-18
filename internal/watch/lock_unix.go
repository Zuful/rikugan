//go:build unix

package watch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// LockFileName is the lock taken in the state directory.
const LockFileName = "lock"

// Lock takes an exclusive lock on the state directory, so that a second
// rikugan serve on the same directory refuses to start instead of
// writing the journal concurrently and sending every notification
// twice. The lock is a safety net for misconfiguration, not the
// guarantee: flock is unreliable on network file systems (NFS, SMB,
// some FUSE), which the state directory must not live on (design §1).
// The returned function releases it.
func Lock(dir string) (func() error, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	path := filepath.Join(dir, LockFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- fixed file name in the operator's state directory
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another rikugan serve instance uses the state directory %s", dir)
		}
		return nil, fmt.Errorf("lock state directory: %w", err)
	}
	return f.Close, nil // closing the file releases the lock
}
