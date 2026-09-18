package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ── Version Management ──

type versionMeta struct {
	Version   int       `json:"version"`
	Timestamp time.Time `json:"timestamp"`
	Size      int64     `json:"size"`
	Hash      string    `json:"hash"`
	GoVersion string    `json:"go_version"`
}

func versionBinPath(n int) string {
	return filepath.Join(versionsDir, fmt.Sprintf("%s.%d", appName, n))
}

func versionMetaPath(n int) string {
	return filepath.Join(versionsDir, fmt.Sprintf("%s.%d.meta", appName, n))
}

func copyFile(src, dst string) error {
	// refuse to copy symlink source, prevent reading tampered files during rollback
	if isSymlink(src) {
		return fmt.Errorf("refuse to copy symlink source: %s", src)
	}
	// refuse to overwrite symlink target, prevent overwriting arbitrary files via symlink
	if isSymlink(dst) {
		return fmt.Errorf("refuse to overwrite symlink target: %s", dst)
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func binaryHash(path string) string {
	// reject symlink, ensure hashing real binary
	f, err := safeOpen(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil))
}

func writeMeta(n int) error {
	binPath := versionBinPath(n)
	info, err := os.Stat(binPath)
	if err != nil {
		return err
	}

	goVer := ""
	if out, err := exec.Command("go", "version").Output(); err == nil {
		goVer = strings.TrimSpace(string(out))
	}

	meta := versionMeta{
		Version:   n,
		Timestamp: time.Now(),
		Size:      info.Size(),
		Hash:      binaryHash(binPath),
		GoVersion: goVer,
	}

	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(versionMetaPath(n), data, 0644)
}

func loadMeta(n int) (*versionMeta, error) {
	data, err := os.ReadFile(versionMetaPath(n))
	if err != nil {
		return nil, err
	}
	var m versionMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// rotateVersions: delete slot 3, shift 2→3, 1→2, current→1
func rotateVersions() {
	os.MkdirAll(versionsDir, 0755)

	// delete oldest
	os.Remove(versionBinPath(maxVersions))
	os.Remove(versionMetaPath(maxVersions))

	// shift back
	for i := maxVersions - 1; i >= 1; i-- {
		os.Rename(versionBinPath(i), versionBinPath(i+1))
		os.Rename(versionMetaPath(i), versionMetaPath(i+1))
	}

	// current binary → .1
	if _, err := os.Stat(appBin); err == nil {
		if err := copyFile(appBin, versionBinPath(1)); err != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] failed to backup current version: %v\n", err)
			return
		}
		writeMeta(1)
	}
}

func verifyBinary(path string) bool {
	ctx := exec.Command(path, "--help")
	out, err := ctx.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] verification failed: %v\n%s\n", err, out)
		return false
	}
	// check output contains program name as marker
	return strings.Contains(string(out), appName)
}

func handleBuild() {
	tmpBin := appBin + ".new" // compile to temp path

	// 1. compile to temp path (inject version info)
	fmt.Println("[" + appName + "] compiling...")
	commitHash := "unknown"
	if out, err := exec.Command("git", "-C", srcDir, "rev-parse", "--short", "HEAD").Output(); err == nil {
		commitHash = strings.TrimSpace(string(out))
	}
	now := time.Now().Format("2006-01-02T15:04:05")
	// Only defaultAppName is baked in; commit/date are persisted to a sidecar
	// .meta file below so CDHash is stable across commits. This matches the
	// main Makefile's strategy — see loadBuildMeta() in main.go.
	ldflags := fmt.Sprintf("-X main.defaultAppName=%s", appName)
	cmd := exec.Command("go", "build", "-ldflags", ldflags, "-o", tmpBin, ".")
	cmd.Dir = srcDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] build failed: %v\n", err)
		os.Remove(tmpBin)
		os.Exit(1)
	}

	// 2. verify new binary (old one not replaced yet)
	fmt.Println("[" + appName + "] verifying new binary...")
	if !verifyBinary(tmpBin) {
		fmt.Fprintln(os.Stderr, "["+appName+"] new binary verification failed")
		os.Remove(tmpBin)
		os.Exit(1)
	}

	// 3. run tests
	fmt.Println("[" + appName + "] running tests...")
	testCmd := exec.Command("go", "test", "-count=1", "-timeout", "60s", "./...")
	testCmd.Dir = srcDir
	testOut, testErr := testCmd.CombinedOutput()
	if testErr != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] tests failed ❌\n%s\n", testOut)
		os.Remove(tmpBin)
		os.Exit(1)
	}
	fmt.Print("[" + appName + "] tests passed ✅\n")

	// 4. backup + rotate (only touch old version after tests pass)
	fmt.Println("[" + appName + "] backing up current version...")
	rotateVersions()

	// 5. atomic replacement: rename is safer than copy, atomic on same filesystem
	if err := os.Rename(tmpBin, appBin); err != nil {
		// rename fails across filesystems, fallback to copy
		if cpErr := copyFile(tmpBin, appBin); cpErr != nil {
			fmt.Fprintf(os.Stderr, "["+appName+"] replacement failed: %v (rename: %v)\n", cpErr, err)
			// rollback
			if _, statErr := os.Stat(versionBinPath(1)); statErr == nil {
				fmt.Println("[" + appName + "] rolling back to previous version...")
				copyFile(versionBinPath(1), appBin)
			}
			os.Remove(tmpBin)
			os.Exit(1)
		}
		os.Remove(tmpBin)
	}

	// Write sidecar .meta so the runtime can surface commit/date without
	// embedding them in the binary. Keeps CDHash stable across commits.
	metaContent := fmt.Sprintf("version=%s\ncommit=%s\ndate=%s\n", buildVersion, commitHash, now)
	if err := os.WriteFile(appBin+".meta", []byte(metaContent), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] warning: failed to write .meta: %v\n", err)
	}

	fmt.Printf("["+appName+"] build successful ✅ (%s)\n", appBin)
	fmt.Printf("["+appName+"] version history saved at %s\n", versionsDir)
}

func handleVersions() {
	for i := 1; i <= maxVersions; i++ {
		meta, err := loadMeta(i)
		if err != nil {
			continue
		}
		binPath := versionBinPath(i)
		info, _ := os.Stat(binPath)
		sizeStr := "?"
		if info != nil {
			sizeStr = fmt.Sprintf("%.1f MB", float64(info.Size())/1024/1024)
		}
		hashStr := meta.Hash
		if len(hashStr) > 12 {
			hashStr = hashStr[:12]
		}
		fmt.Printf("  .%d  %s  %s  %s\n", i, meta.Timestamp.Format("2006-01-02 15:04:05"), sizeStr, hashStr)
	}
	if _, err := loadMeta(1); err != nil {
		fmt.Println("  (no saved versions)")
	}
}

func handleUpdate() {
	// 1. git pull
	fmt.Println("[" + appName + "] git pull...")
	pull := exec.Command("git", "-C", srcDir, "pull", "--ff-only")
	pull.Stdout = os.Stdout
	pull.Stderr = os.Stderr
	if err := pull.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] git pull failed: %v\n", err)
		fmt.Fprintln(os.Stderr, "["+appName+"] resolve manually then re-run "+appName+" update")
		os.Exit(1)
	}

	// 2. call handleBuild (backup → compile → verify → rollback on failure)
	handleBuild()
}

func handleRollback(n int) {
	binPath := versionBinPath(n)
	if _, err := os.Stat(binPath); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] version .%d not found\n", n)
		os.Exit(1)
	}

	meta, _ := loadMeta(n)
	if meta != nil {
		fmt.Printf("["+appName+"] rolling back to version .%d (%s)\n", n, meta.Timestamp.Format("2006-01-02 15:04:05"))
	}

	if err := copyFile(binPath, appBin); err != nil {
		fmt.Fprintf(os.Stderr, "["+appName+"] rollback failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("["+appName+"] rollback successful ✅ → %s\n", appBin)
}
