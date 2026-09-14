package cloud

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// File names under the data directory.
const (
	accountFile   = "account.json"
	machineIDFile = "machine-id"
)

// LoadAccount reads the stored account; (nil, nil) when signed out.
func LoadAccount(dataDir string) (*Account, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, accountFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read account: %w", err)
	}
	var a Account
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("read account %s: %w", filepath.Join(dataDir, accountFile), err)
	}
	if a.Token == "" || a.Email == "" {
		return nil, fmt.Errorf("read account %s: missing email or token; run `agentflow account login`", filepath.Join(dataDir, accountFile))
	}
	return &a, nil
}

// SaveAccount stores the account with owner-only permissions.
func SaveAccount(dataDir string, a *Account) error {
	if a == nil {
		return errors.New("save account: nil account")
	}
	b, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(dataDir, accountFile, append(b, '\n'))
}

// DeleteAccount removes the stored account.
func DeleteAccount(dataDir string) error {
	err := os.Remove(filepath.Join(dataDir, accountFile))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("delete account: %w", err)
	}
	return nil
}

// MachineID returns this machine's stable random id, creating it on first
// use (never the hostname).
func MachineID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, machineIDFile)
	if id, err := readMachineID(path); err == nil || !errors.Is(err, fs.ErrNotExist) {
		return id, err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return "", fmt.Errorf("machine id: %w", err)
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b[:])
	// Write the id to a temp file, then hard-link it into place: the link
	// fails if another process got there first, and nobody ever sees a
	// partly written file.
	tmp, err := os.CreateTemp(dataDir, ".machine-id.*")
	if err != nil {
		return "", fmt.Errorf("machine id: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("machine id: %w", err)
	}
	if _, err := tmp.WriteString(id + "\n"); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("machine id: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("machine id: %w", err)
	}
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return readMachineID(path)
		}
		return "", fmt.Errorf("machine id: %w", err)
	}
	return id, nil
}

func readMachineID(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if len(id) != 32 {
		return "", fmt.Errorf("machine id in %s is malformed; delete the file to create a new one", path)
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", fmt.Errorf("machine id in %s is malformed; delete the file to create a new one", path)
	}
	return id, nil
}

// writeFileAtomic replaces dir/name with data (mode 0600) so a crash leaves
// either the old file or the new one, never a partial token.
func writeFileAtomic(dir, name string, data []byte) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}
