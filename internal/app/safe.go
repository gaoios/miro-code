package app

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// shortID safely truncates an ID string to n chars (default 8).
func shortID(id string, n ...int) string {
	limit := 8
	if len(n) > 0 && n[0] > 0 {
		limit = n[0]
	}
	if len(id) <= limit {
		return id
	}
	return id[:limit]
}

// ── Symlink Protection ──

// isSymlink checks if path is a symlink (does not follow)
func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeSymlink != 0
}

// lstatSafe returns Lstat result, rejects symlinks.
// Returns (info, nil) for safe regular files/directories,
// returns (nil, err) for non-existent or symlink paths.
func lstatSafe(path string) (os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("rejecting symlink: %s", path)
	}
	return info, nil
}

// safeReadFile reads file contents, rejects symlinks.
// Prevents injecting arbitrary file contents into prompt via symlink.
func safeReadFile(path string) ([]byte, error) {
	if isSymlink(path) {
		return nil, fmt.Errorf("refusing to read symlink: %s", path)
	}
	return os.ReadFile(path)
}

// safeOpen opens a file, rejects symlinks.
func safeOpen(path string) (*os.File, error) {
	if isSymlink(path) {
		return nil, fmt.Errorf("refusing to open symlink: %s", path)
	}
	return os.Open(path)
}

// ── Recursive scan protection (symlink loop prevention) ──

// deviceInode uniquely identifies a file/directory (NFS-mount safe)
type deviceInode struct {
	dev uint64
	ino uint64
}

// getDeviceInode gets the (device, inode) pair of a file
func getDeviceInode(path string) (deviceInode, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return deviceInode{}, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return deviceInode{}, false
	}
	return deviceInode{dev: uint64(stat.Dev), ino: stat.Ino}, true
}

// ── NFS-safe lock ──

// mkdirLock implements an atomic lock using mkdir (NFS-safe).
// mkdir is atomic on all filesystems, unlike O_EXCL which can race on NFS.
// Returns true if lock acquired, false if already held.
func mkdirLock(path string) bool {
	err := os.Mkdir(path, 0700)
	if err != nil {
		return false // already exists
	}
	// write PID to file in lock dir for stale detection
	pidFile := filepath.Join(path, "pid")
	os.WriteFile(pidFile, []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
	return true
}

// mkdirUnlock releases the mkdir lock
func mkdirUnlock(path string) {
	os.RemoveAll(path)
}

// readLockPid reads the PID from a mkdir lock
func readLockPid(path string) (int, bool) {
	pidFile := filepath.Join(path, "pid")
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return 0, false
	}
	var pid int
	if _, err := fmt.Sscanf(string(data), "%d", &pid); err != nil {
		return 0, false
	}
	return pid, true
}
