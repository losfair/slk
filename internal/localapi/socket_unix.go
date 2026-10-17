//go:build linux || darwin

package localapi

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

func owned(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
func privateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || !owned(info) || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("local API directory must be owned by this user with mode 0700: %s", path)
	}
	return nil
}
func checkPrivateFile(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !owned(info) || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("local API file must be regular, owned by this user, and mode 0600: %s", path)
	}
	return nil
}

// Reject symlinks in configured ancestors before creating directories. Ancestors
// need not all be private (e.g. /tmp), but must prevent other users from
// replacing our directory. The socket's own directory must be private.
func noSymlinks(path string) error {
	for path != "/" {
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("local API path contains a symlink: %s", path)
			}
			stat, ok := info.Sys().(*syscall.Stat_t)
			if !ok || stat.Uid != 0 && !owned(info) {
				return fmt.Errorf("local API ancestor must be owned by this user or root: %s", path)
			}
			if info.Mode().Perm()&0o022 != 0 && info.Mode()&os.ModeSticky == 0 {
				return fmt.Errorf("local API ancestor is writable by other users without the sticky bit: %s", path)
			}
		}
		path = filepath.Dir(path)
	}
	return nil
}
func prepareSocket(path string) (string, *os.File, error) {
	if path == "" {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if !filepath.IsAbs(base) || privateDir(base) != nil {
			base = ""
		}
		if base == "" {
			var err error
			base, err = filepath.EvalSymlinks(os.TempDir())
			if err != nil {
				return "", nil, err
			}
			base = filepath.Join(base, "slk-"+strconv.Itoa(os.Getuid()))
		} else {
			base = filepath.Join(base, "slk")
		}
		path = filepath.Join(base, "api.sock")
	}
	if !filepath.IsAbs(path) {
		return "", nil, errors.New("local API socket_path must be absolute")
	}
	path = filepath.Clean(path)
	if name := filepath.Base(path); name == "api.json" || name == "api.lock" {
		return "", nil, errors.New("local API socket_path must not use the reserved name api.json or api.lock")
	}
	if len(path) > 100 {
		return "", nil, errors.New("local API socket path is too long; configure socket_path with at most 100 bytes")
	}
	dir := filepath.Dir(path)
	if err := noSymlinks(dir); err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	if err := privateDir(dir); err != nil {
		return "", nil, err
	}
	lockPath := filepath.Join(dir, "api.lock")
	if err := checkPrivateFile(lockPath); err != nil {
		return "", nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	fail := func(err error) (string, *os.File, error) { _ = lock.Close(); return "", nil, err }
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("local API is already running in %s: %w", dir, err))
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSocket == 0 || !owned(info) {
			return fail(fmt.Errorf("refusing to remove non-socket or foreign path: %s", path))
		}
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		if dialErr == nil {
			_ = conn.Close()
			return fail(fmt.Errorf("local API socket is already in use: %s", path))
		}
		if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, os.ErrNotExist) {
			return fail(fmt.Errorf("checking local API socket: %w", dialErr))
		}
		if err = os.Remove(path); err != nil {
			return fail(err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fail(err)
	}
	return path, lock, nil
}
