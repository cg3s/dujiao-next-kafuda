package logger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveLogFilePathDefaultDir(t *testing.T) {
	tmpDir := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd failed: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldWD)
	})
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("chdir failed: %v", err)
	}

	got, err := resolveLogFilePath(Options{})
	if err != nil {
		t.Fatalf("resolve default log path failed: %v", err)
	}

	expectedRoot, err := os.Stat(tmpDir)
	if err != nil {
		t.Fatalf("stat temp dir: %v", err)
	}
	gotDir := filepath.Dir(got)
	gotRoot, err := os.Stat(filepath.Dir(gotDir))
	if err != nil {
		t.Fatalf("stat log root: %v", err)
	}
	if filepath.Base(gotDir) != defaultLogDirName || !os.SameFile(gotRoot, expectedRoot) {
		t.Fatalf("unexpected log dir: got=%s expected=%s", gotDir, filepath.Join(tmpDir, defaultLogDirName))
	}
	if filepath.Base(got) != defaultLogFilename {
		t.Fatalf("unexpected log filename: %s", filepath.Base(got))
	}
	if _, err := os.Stat(filepath.Dir(got)); err != nil {
		t.Fatalf("expected log dir to be created: %v", err)
	}
}

func TestNewReleaseWritesToConfiguredFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Options{
		Dir:      tmpDir,
		Filename: "release.log",
	}
	log, closeLog := newLogger("release", cfg)
	t.Cleanup(func() {
		if err := closeLog(); err != nil {
			t.Errorf("close release log: %v", err)
		}
	})
	log.Info("release-log-test")
	_ = log.Sync()

	content, err := os.ReadFile(filepath.Join(tmpDir, "release.log"))
	if err != nil {
		t.Fatalf("read release log failed: %v", err)
	}
	if !strings.Contains(string(content), "release-log-test") {
		t.Fatalf("expected log content to contain message, got=%s", string(content))
	}
}

func TestNewDebugDoesNotWriteFile(t *testing.T) {
	tmpDir := t.TempDir()
	cfg := Options{
		Dir:      tmpDir,
		Filename: "debug.log",
	}
	log := New("debug", cfg)
	log.Info("debug-log-test")
	_ = log.Sync()

	if _, err := os.Stat(filepath.Join(tmpDir, "debug.log")); !os.IsNotExist(err) {
		t.Fatalf("debug mode should not create log file")
	}
}
