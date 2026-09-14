package cloud

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestAccountRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if a, err := LoadAccount(dir); a != nil || err != nil {
		t.Fatalf("signed out: %+v, %v", a, err)
	}
	if err := DeleteAccount(dir); err != nil {
		t.Fatalf("delete without account: %v", err)
	}

	want := &Account{Email: "a@example.com", Token: "tok", MachineID: "0123456789abcdef0123456789abcdef", VerifiedAt: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)}
	if err := SaveAccount(dir, want); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(filepath.Join(dir, "account.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("account.json mode %v, want 0600", st.Mode().Perm())
	}
	if dst, _ := os.Stat(dir); dst.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode %v, want 0700", dst.Mode().Perm())
	}
	got, err := LoadAccount(dir)
	if err != nil || *got != *want {
		t.Fatalf("load = %+v, %v", got, err)
	}

	// Overwrite leaves no temp files behind.
	want.Token = "tok2"
	if err := SaveAccount(dir, want); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("data dir has %d entries after two saves, want only account.json", len(entries))
	}
	if got, _ := LoadAccount(dir); got.Token != "tok2" {
		t.Fatalf("token after overwrite = %q", got.Token)
	}

	if err := DeleteAccount(dir); err != nil {
		t.Fatal(err)
	}
	if a, err := LoadAccount(dir); a != nil || err != nil {
		t.Fatalf("after delete: %+v, %v", a, err)
	}
}

func TestLoadAccountRejectsDamagedFile(t *testing.T) {
	dir := t.TempDir()
	for _, content := range []string{"{not json", `{"email":"a@example.com"}`} {
		if err := os.WriteFile(filepath.Join(dir, "account.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if a, err := LoadAccount(dir); err == nil {
			t.Errorf("content %q loaded as %+v", content, a)
		}
	}
}

func TestMachineIDIsCreatedOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	id, err := MachineID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, err := hex.DecodeString(id); err != nil || len(b) != 16 {
		t.Fatalf("id %q is not 16 bytes of hex", id)
	}
	host, _ := os.Hostname()
	if id == host {
		t.Fatal("machine id is the hostname")
	}
	st, err := os.Stat(filepath.Join(dir, "machine-id"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("machine-id file: %v, %v", st, err)
	}
	again, err := MachineID(dir)
	if err != nil || again != id {
		t.Fatalf("second call = %q, %v; want %q", again, err, id)
	}

	other, err := MachineID(t.TempDir())
	if err != nil || other == id {
		t.Fatalf("another data dir got %q, %v", other, err)
	}
}

func TestMachineIDConcurrentFirstUse(t *testing.T) {
	dir := t.TempDir()
	var wg sync.WaitGroup
	ids := make(chan string, 16)
	for range 16 {
		wg.Go(func() {
			id, err := MachineID(dir)
			if err != nil {
				t.Error(err)
			}
			ids <- id
		})
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent first use produced %q and %q", first, id)
		}
	}
}

func TestMachineIDRejectsDamagedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "machine-id"), []byte("my-laptop\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if id, err := MachineID(dir); err == nil {
		t.Fatalf("damaged machine-id accepted as %q", id)
	}
}
