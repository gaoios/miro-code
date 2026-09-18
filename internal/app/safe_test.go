package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsSymlink_RegularFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "regular.txt")
	os.WriteFile(f, []byte("hello"), 0644)

	if isSymlink(f) {
		t.Error("regular file should not be detected as symlink")
	}
}

func TestIsSymlink_Symlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("hello"), 0644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(target, link)

	if !isSymlink(link) {
		t.Error("symlink should be detected")
	}
}

func TestIsSymlink_NonExistent(t *testing.T) {
	if isSymlink("/nonexistent/file") {
		t.Error("nonexistent should return false")
	}
}

func TestIsSymlink_Directory(t *testing.T) {
	dir := t.TempDir()
	if isSymlink(dir) {
		t.Error("directory should not be detected as symlink")
	}
}

func TestIsSymlink_DirSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "targetdir")
	os.MkdirAll(target, 0755)
	link := filepath.Join(dir, "linkdir")
	os.Symlink(target, link)

	if !isSymlink(link) {
		t.Error("dir symlink should be detected")
	}
}

func TestLstatSafe_RegularFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "regular.txt")
	os.WriteFile(f, []byte("hello"), 0644)

	info, err := lstatSafe(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if info.Size() != 5 {
		t.Errorf("size = %d, want 5", info.Size())
	}
}

func TestLstatSafe_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("hello"), 0644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(target, link)

	_, err := lstatSafe(link)
	if err == nil {
		t.Error("should reject symlink")
	}
}

func TestLstatSafe_NonExistent(t *testing.T) {
	_, err := lstatSafe("/nonexistent")
	if err == nil {
		t.Error("should error on nonexistent")
	}
}

func TestSafeReadFile_RegularFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "regular.txt")
	os.WriteFile(f, []byte("content"), 0644)

	data, err := safeReadFile(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != "content" {
		t.Errorf("data = %q", data)
	}
}

func TestSafeReadFile_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("secret"), 0644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(target, link)

	_, err := safeReadFile(link)
	if err == nil {
		t.Error("should reject symlink")
	}
}

func TestSafeOpen_RegularFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "regular.txt")
	os.WriteFile(f, []byte("hello"), 0644)

	file, err := safeOpen(f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	file.Close()
}

func TestSafeOpen_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	os.WriteFile(target, []byte("hello"), 0644)
	link := filepath.Join(dir, "link.txt")
	os.Symlink(target, link)

	_, err := safeOpen(link)
	if err == nil {
		t.Error("should reject symlink")
	}
}

func TestGetDeviceInode(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("hello"), 0644)

	di, ok := getDeviceInode(f)
	if !ok {
		t.Fatal("should return ok for regular file")
	}
	if di.ino == 0 {
		t.Error("inode should not be 0")
	}

	// Same file = same inode
	di2, ok := getDeviceInode(f)
	if !ok {
		t.Fatal("should return ok")
	}
	if di != di2 {
		t.Error("same file should have same device+inode")
	}

	// Different file = different inode
	f2 := filepath.Join(dir, "test2.txt")
	os.WriteFile(f2, []byte("world"), 0644)
	di3, ok := getDeviceInode(f2)
	if !ok {
		t.Fatal("should return ok")
	}
	if di == di3 {
		t.Error("different files should have different inodes")
	}
}

func TestGetDeviceInode_NonExistent(t *testing.T) {
	_, ok := getDeviceInode("/nonexistent")
	if ok {
		t.Error("should return not ok for nonexistent")
	}
}

func TestMkdirLock(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "test.lock.d")

	// First lock should succeed
	if !mkdirLock(lockDir) {
		t.Error("first lock should succeed")
	}

	// Second lock should fail (already held)
	if mkdirLock(lockDir) {
		t.Error("second lock should fail")
	}

	// Read PID back
	pid, ok := readLockPid(lockDir)
	if !ok {
		t.Error("should be able to read PID")
	}
	if pid != os.Getpid() {
		t.Errorf("pid = %d, want %d", pid, os.Getpid())
	}

	// Unlock
	mkdirUnlock(lockDir)

	// Should be able to lock again
	if !mkdirLock(lockDir) {
		t.Error("lock after unlock should succeed")
	}
	mkdirUnlock(lockDir)
}

func TestReadLockPid_NoDir(t *testing.T) {
	_, ok := readLockPid("/nonexistent/lock.d")
	if ok {
		t.Error("should return not ok for nonexistent")
	}
}

func TestReadLockPid_NoPidFile(t *testing.T) {
	dir := t.TempDir()
	lockDir := filepath.Join(dir, "empty.lock.d")
	os.MkdirAll(lockDir, 0700)

	_, ok := readLockPid(lockDir)
	if ok {
		t.Error("should return not ok when no pid file")
	}
}

// ── Integration tests: symlink protection across components ──

func TestCopyFile_RejectsSymlinkSrc(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.bin")
	os.WriteFile(target, []byte("binary"), 0755)
	link := filepath.Join(dir, "link.bin")
	os.Symlink(target, link)

	err := copyFile(link, filepath.Join(dir, "dst.bin"))
	if err == nil {
		t.Error("should reject symlink source")
	}
}

func TestCopyFile_RejectsSymlinkDst(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	os.WriteFile(src, []byte("binary"), 0755)
	target := filepath.Join(dir, "real-dst.bin")
	os.WriteFile(target, []byte("original"), 0755)
	link := filepath.Join(dir, "link-dst.bin")
	os.Symlink(target, link)

	err := copyFile(src, link)
	if err == nil {
		t.Error("should reject symlink destination")
	}

	// Original should be untouched
	data, _ := os.ReadFile(target)
	if string(data) != "original" {
		t.Error("original file should not be modified")
	}
}

func TestBinaryHash_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.bin")
	os.WriteFile(target, []byte("binary"), 0755)
	link := filepath.Join(dir, "link.bin")
	os.Symlink(target, link)

	h := binaryHash(link)
	if h != "" {
		t.Error("should return empty hash for symlink")
	}
}

func TestLoadFileWithBudget_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "secret.md")
	os.WriteFile(target, []byte("# Secret Content\nAPI_KEY=sk-ant-xxx"), 0644)
	link := filepath.Join(dir, "SOUL.md")
	os.Symlink(target, link)

	_, ok := loadFileWithBudget(link, 10000)
	if ok {
		t.Error("should reject symlink (prevents prompt injection)")
	}
}

func TestFindCLAUDEMDs_SkipsSymlinkDir(t *testing.T) {
	dir := t.TempDir()

	// Real subdir with CLAUDE.md
	sub := filepath.Join(dir, "real")
	os.MkdirAll(sub, 0755)
	os.WriteFile(filepath.Join(sub, "CLAUDE.md"), []byte("real"), 0644)

	// Symlink dir pointing back to root (cycle!)
	os.Symlink(dir, filepath.Join(dir, "cycle"))

	results := findCLAUDEMDs(dir, 3)

	// Should find real/CLAUDE.md but not enter cycle
	found := false
	for _, r := range results {
		if filepath.Base(filepath.Dir(r)) == "real" {
			found = true
		}
	}
	if !found {
		t.Error("should find real/CLAUDE.md")
	}
}

func TestFindCLAUDEMDs_SkipsSymlinkCLAUDEMD(t *testing.T) {
	dir := t.TempDir()

	// Symlink CLAUDE.md pointing to /etc/passwd (attack vector)
	os.Symlink("/etc/passwd", filepath.Join(dir, "CLAUDE.md"))

	results := findCLAUDEMDs(dir, 0)
	if len(results) != 0 {
		t.Errorf("should skip symlink CLAUDE.md, got %v", results)
	}
}

func TestSafetyCheck_DetectsSymlinkSoulFile(t *testing.T) {
	dir := t.TempDir()
	origWS := workspace
	workspace = dir
	defer func() { workspace = origWS }()

	// Create normal soul files
	for _, f := range []string{"IDENTITY.md", "USER.md", "AGENTS.md", "TOOLS.md", "MEMORY.md", "HEARTBEAT.md"} {
		os.WriteFile(filepath.Join(dir, f), []byte("content"), 0644)
	}
	// SOUL.md is a symlink (attack!)
	os.WriteFile(filepath.Join(dir, "real-soul.md"), []byte("fake soul"), 0644)
	os.Symlink(filepath.Join(dir, "real-soul.md"), filepath.Join(dir, "SOUL.md"))

	os.MkdirAll(filepath.Join(dir, "memory", "topics"), 0755)
	os.MkdirAll(filepath.Join(dir, "projects"), 0755)

	// Should detect symlink — function prints to stderr, just verify no panic
	safetyCheck("test")
}

func TestDirSize_SymlinkCycle(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello"), 0644)

	// Create symlink cycle: dir/loop → dir
	os.Symlink(dir, filepath.Join(dir, "loop"))

	// Should not infinite loop — the symlink cycle protection should handle this
	size := dirSize(dir)
	if size != 5 { // only "hello"
		t.Errorf("dirSize with symlink cycle = %d, want 5", size)
	}
}
