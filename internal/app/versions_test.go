package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "dst.bin")

	content := []byte("hello binary world")
	os.WriteFile(src, content, 0755)

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile failed: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("content mismatch: %q vs %q", got, content)
	}
}

func TestCopyFile_NonExistentSrc(t *testing.T) {
	dir := t.TempDir()
	err := copyFile(filepath.Join(dir, "nope"), filepath.Join(dir, "dst"))
	if err == nil {
		t.Error("expected error for nonexistent src")
	}
}

func TestBinaryHash(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.bin")
	os.WriteFile(f, []byte("test data"), 0644)

	h := binaryHash(f)
	if h == "" {
		t.Error("hash is empty")
	}
	if len(h) != 64 { // sha256 hex
		t.Errorf("hash length = %d, want 64", len(h))
	}

	// deterministic
	h2 := binaryHash(f)
	if h != h2 {
		t.Error("hash not deterministic")
	}
}

func TestBinaryHash_NonExistent(t *testing.T) {
	h := binaryHash("/nonexistent/file")
	if h != "" {
		t.Errorf("expected empty hash, got %q", h)
	}
}

func TestVersionBinPath(t *testing.T) {
	origDir := versionsDir
	versionsDir = "/tmp/test-versions"
	defer func() { versionsDir = origDir }()

	p := versionBinPath(1)
	if p != "/tmp/test-versions/"+appName+".1" {
		t.Errorf("got %q", p)
	}
	p = versionBinPath(3)
	if p != "/tmp/test-versions/"+appName+".3" {
		t.Errorf("got %q", p)
	}
}

func TestVersionMetaPath(t *testing.T) {
	origDir := versionsDir
	versionsDir = "/tmp/test-versions"
	defer func() { versionsDir = origDir }()

	p := versionMetaPath(2)
	if p != "/tmp/test-versions/"+appName+".2.meta" {
		t.Errorf("got %q", p)
	}
}

func TestWriteAndLoadMeta(t *testing.T) {
	dir := t.TempDir()
	origDir := versionsDir
	versionsDir = dir
	defer func() { versionsDir = origDir }()

	// create a fake binary for version 1
	binPath := versionBinPath(1)
	os.WriteFile(binPath, []byte("fake binary"), 0755)

	if err := writeMeta(1); err != nil {
		t.Fatalf("writeMeta failed: %v", err)
	}

	meta, err := loadMeta(1)
	if err != nil {
		t.Fatalf("loadMeta failed: %v", err)
	}

	if meta.Version != 1 {
		t.Errorf("version = %d, want 1", meta.Version)
	}
	if meta.Size != 11 { // len("fake binary")
		t.Errorf("size = %d, want 11", meta.Size)
	}
	if meta.Hash == "" {
		t.Error("hash is empty")
	}
	if meta.GoVersion == "" {
		t.Error("go version is empty")
	}
}

func TestLoadMeta_NonExistent(t *testing.T) {
	dir := t.TempDir()
	origDir := versionsDir
	versionsDir = dir
	defer func() { versionsDir = origDir }()

	_, err := loadMeta(99)
	if err == nil {
		t.Error("expected error for nonexistent meta")
	}
}

func TestLoadMeta_BadJSON(t *testing.T) {
	dir := t.TempDir()
	origDir := versionsDir
	versionsDir = dir
	defer func() { versionsDir = origDir }()

	os.WriteFile(versionMetaPath(1), []byte("not json"), 0644)
	_, err := loadMeta(1)
	if err == nil {
		t.Error("expected error for bad JSON")
	}
}

func TestRotateVersions(t *testing.T) {
	dir := t.TempDir()
	origDir := versionsDir
	origBin := appBin
	versionsDir = dir
	appBin = filepath.Join(dir, appName+"-current")
	defer func() {
		versionsDir = origDir
		appBin = origBin
	}()

	// create fake current binary
	os.WriteFile(appBin, []byte("current"), 0755)

	// create existing version .1
	os.WriteFile(versionBinPath(1), []byte("old-v1"), 0755)
	os.WriteFile(versionMetaPath(1), []byte(`{"version":1}`), 0644)

	rotateVersions()

	// current → .1
	data, err := os.ReadFile(versionBinPath(1))
	if err != nil {
		t.Fatalf("v1 missing: %v", err)
	}
	if string(data) != "current" {
		t.Errorf("v1 = %q, want 'current'", data)
	}

	// old .1 → .2
	data, err = os.ReadFile(versionBinPath(2))
	if err != nil {
		t.Fatalf("v2 missing: %v", err)
	}
	if string(data) != "old-v1" {
		t.Errorf("v2 = %q, want 'old-v1'", data)
	}
}

func TestRotateVersions_DropsOldest(t *testing.T) {
	dir := t.TempDir()
	origDir := versionsDir
	origBin := appBin
	origMax := maxVersions
	versionsDir = dir
	appBin = filepath.Join(dir, appName+"-current")
	maxVersions = 2 // smaller for test
	defer func() {
		versionsDir = origDir
		appBin = origBin
		maxVersions = origMax
	}()

	os.WriteFile(appBin, []byte("current"), 0755)
	os.WriteFile(versionBinPath(1), []byte("v1"), 0755)
	os.WriteFile(versionBinPath(2), []byte("v2-oldest"), 0755)

	rotateVersions()

	// v2 (oldest) should be deleted (it was maxVersions)
	// v1 → v2
	data, _ := os.ReadFile(versionBinPath(2))
	if string(data) != "v1" {
		t.Errorf("v2 = %q, want 'v1'", data)
	}

	// current → v1
	data, _ = os.ReadFile(versionBinPath(1))
	if string(data) != "current" {
		t.Errorf("v1 = %q, want 'current'", data)
	}
}

func TestVerifyBinary(t *testing.T) {
	// Create a fake script that outputs the app name
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "fake-bin")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho '"+appName+" test'\n"), 0755)

	if !verifyBinary(fakeBin) {
		t.Errorf("expected verification to pass for binary outputting %q", appName)
	}
}

func TestVerifyBinary_Bad(t *testing.T) {
	dir := t.TempDir()
	fakeBin := filepath.Join(dir, "bad-bin")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho 'nope'\n"), 0755)

	if verifyBinary(fakeBin) {
		t.Error("expected verification to fail")
	}
}

func TestVerifyBinary_NonExistent(t *testing.T) {
	if verifyBinary("/nonexistent/binary") {
		t.Error("expected verification to fail for nonexistent")
	}
}

func TestVersionMetaRoundTrip(t *testing.T) {
	dir := t.TempDir()
	origDir := versionsDir
	versionsDir = dir
	defer func() { versionsDir = origDir }()

	// Write a meta manually and verify round-trip
	meta := versionMeta{
		Version:   2,
		Size:      12345,
		Hash:      "abc123def456",
		GoVersion: "go1.22.0",
	}
	data, _ := json.MarshalIndent(meta, "", "  ")
	os.WriteFile(versionMetaPath(2), data, 0644)

	loaded, err := loadMeta(2)
	if err != nil {
		t.Fatalf("loadMeta: %v", err)
	}
	if loaded.Version != 2 || loaded.Size != 12345 || loaded.Hash != "abc123def456" {
		t.Errorf("meta mismatch: %+v", loaded)
	}
}
