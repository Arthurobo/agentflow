package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstanceLockRejectsSecondDaemon(t *testing.T) {
	db := filepath.Join(t.TempDir(), "data", "agentflow.db")
	first, err := acquireInstanceLock(db)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if first.path != db+".lock" {
		t.Fatalf("lock path = %s", first.path)
	}

	_, err = acquireInstanceLock(db)
	if err == nil {
		t.Fatal("second lock on the same database succeeded")
	}
	want := fmt.Sprintf("agentflow is already running (pid %d; lock %s)", os.Getpid(), db+".lock")
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}

	first.Release()
	second, err := acquireInstanceLock(db)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	second.Release()
}

func TestInstanceLockIsPerDatabase(t *testing.T) {
	dir := t.TempDir()
	a, err := acquireInstanceLock(filepath.Join(dir, "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	b, err := acquireInstanceLock(filepath.Join(dir, "b.db"))
	if err != nil {
		t.Fatalf("a different database was blocked: %v", err)
	}
	b.Release()
	data, _ := os.ReadFile(filepath.Join(dir, "a.db.lock"))
	if strings.TrimSpace(string(data)) != fmt.Sprint(os.Getpid()) {
		t.Fatalf("lock file content = %q", data)
	}
}

func TestDaemonHoldsLockSeesARunningDaemonWithoutCreatingTheFile(t *testing.T) {
	db := filepath.Join(t.TempDir(), "agentflow.db")
	if daemonHoldsLock(db) {
		t.Fatal("no lock file, but a daemon was reported")
	}
	if _, err := os.Stat(lockPathFor(db)); !os.IsNotExist(err) {
		t.Fatalf("checking created the lock file: %v", err)
	}
	l, err := acquireInstanceLock(db)
	if err != nil {
		t.Fatal(err)
	}
	if !daemonHoldsLock(db) {
		t.Fatal("a held lock was not reported")
	}
	l.Release()
	if daemonHoldsLock(db) {
		t.Fatal("a released lock was still reported")
	}
}
