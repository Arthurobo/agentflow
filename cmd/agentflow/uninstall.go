package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arthurobo/agentflow/internal/adminsock"
	"github.com/arthurobo/agentflow/internal/cloud"
	"github.com/arthurobo/agentflow/internal/remote"
	"github.com/arthurobo/agentflow/internal/remote/cloudflared"
	"github.com/arthurobo/agentflow/internal/store"
	"github.com/arthurobo/agentflow/internal/uploads"
)

// uninstaller removes agentflow. It only ever deletes paths it can name:
// with AF_DB=$HOME/agentflow.db the data "directory" is the home directory,
// so nothing here removes a directory tree derived from AF_DB's location.
type uninstaller struct {
	in          io.Reader
	out         io.Writer
	cfg         config
	home        string
	exe         string // resolved path of this binary
	svc         serviceManager
	admin       *adminsock.Client
	uploadsRoot string
	envFile     string
}

func runUninstall(cfg config, purge, yes bool) int {
	home, _ := os.UserHomeDir()
	exe, err := resolveExecutable()
	if err != nil {
		exe = ""
	}
	u := &uninstaller{
		in:          os.Stdin,
		out:         os.Stdout,
		cfg:         cfg,
		home:        home,
		exe:         exe,
		svc:         newServiceManager(),
		admin:       adminClient(cfg),
		uploadsRoot: uploads.Root(),
		envFile:     envFilePath(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := u.run(ctx, purge, yes); err != nil {
		if errors.Is(err, errAborted) {
			fmt.Println("Nothing was removed.")
			return 1
		}
		fmt.Fprintln(os.Stderr, "agentflow uninstall:", err)
		return 1
	}
	return 0
}

// binaryRemovable reports whether exe is inside ~/.local/bin, the only place
// agentflow installs itself. A binary anywhere else (a package manager, a
// build tree) is left for its owner.
func (u *uninstaller) binaryRemovable() bool {
	if u.home == "" || u.exe == "" {
		return false
	}
	dir := filepath.Join(u.home, ".local", "bin")
	rel, err := filepath.Rel(dir, u.exe)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !strings.Contains(rel, string(filepath.Separator))
}

// purgeFiles are the individual files --purge deletes.
func (u *uninstaller) purgeFiles() []string {
	db := u.cfg.dbPath
	files := []string{
		db, db + "-wal", db + "-shm",
		lockPathFor(db),
		filepath.Join(u.cfg.dataDir, "hook.secret"),
		filepath.Join(u.cfg.dataDir, remote.StatusFileName),
		filepath.Join(u.cfg.dataDir, "account.json"),
		filepath.Join(u.cfg.dataDir, "machine-id"),
		filepath.Join(u.cfg.dataDir, remote.ServeRecordName),
		filepath.Join(u.cfg.dataDir, cloud.TunnelFileName),
		cloudflared.NewInstaller(u.cfg.dataDir).Path(),
		adminsock.SocketPath(u.cfg.dataDir),
	}
	// The reference copy of the built-in defaults, file by file.
	for _, name := range store.DefaultsFileNames {
		files = append(files, filepath.Join(u.defaultsDir(), name))
	}
	if u.envFile != "" {
		files = append(files, u.envFile)
	}
	return files
}

func (u *uninstaller) run(ctx context.Context, purge, yes bool) error {
	fmt.Fprintln(u.out, "This will:")
	if u.svc.Supported() {
		fmt.Fprintln(u.out, "  - stop and remove the agentflow service")
	}
	if u.binaryRemovable() {
		fmt.Fprintf(u.out, "  - delete %s\n", u.exe)
	} else if u.exe != "" {
		fmt.Fprintf(u.out, "  - leave the binary at %s (not installed by agentflow; remove it yourself)\n", u.exe)
	}
	if purge {
		fmt.Fprintln(u.out, "  - log agentflow's own Tailscale node out (if it runs one) and delete agentflow's data:")
		for _, p := range u.purgeFiles() {
			fmt.Fprintf(u.out, "      %s\n", p)
		}
		fmt.Fprintf(u.out, "      %s\n", u.tailscaleDir())
		fmt.Fprintf(u.out, "      %s\n", u.uploadsRoot)
	} else {
		fmt.Fprintf(u.out, "  - keep your data in %s (use --purge to delete it)\n", u.cfg.dataDir)
	}
	if !yes {
		fmt.Fprint(u.out, "Continue? [y/N] ")
		line, _ := bufio.NewReader(u.in).ReadString('\n')
		if ans := strings.ToLower(strings.TrimSpace(line)); ans != "y" && ans != "yes" {
			return errAborted
		}
	}

	// Log out while the daemon still runs, so the node leaves the tailnet
	// cleanly instead of lingering as an offline machine with valid keys.
	// Only agentflow's own node: the computer's Tailscale isn't agentflow's
	// to sign out.
	if purge && u.runsEmbeddedNode(ctx) {
		switch err := u.admin.Do(ctx, "POST", "/remote/logout", nil); {
		case err == nil:
			fmt.Fprintln(u.out, "Logged out of Tailscale.")
		case errors.Is(err, adminsock.ErrNotRunning):
		default:
			fmt.Fprintf(u.out, "Could not log out of Tailscale (%v); remove the machine at %s.\n", err, remote.MachineAuthURL)
		}
	}

	if u.svc.Supported() {
		if err := u.svc.Remove(ctx); err != nil {
			return fmt.Errorf("remove the service: %w", err)
		}
		fmt.Fprintln(u.out, "Removed the service.")
	}
	if u.binaryRemovable() {
		if err := os.Remove(u.exe); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("delete %s: %w", u.exe, err)
		}
		fmt.Fprintf(u.out, "Deleted %s.\n", u.exe)
	}

	if !purge {
		fmt.Fprintf(u.out, "Your data is still in %s. Run `agentflow uninstall --purge` from a reinstalled binary to delete it.\n", u.cfg.dataDir)
		u.leftovers()
		return nil
	}
	u.purge()
	u.leftovers()
	return nil
}

// runsEmbeddedNode reports whether the running daemon serves remote access
// through agentflow's own Tailscale node.
func (u *uninstaller) runsEmbeddedNode(ctx context.Context) bool {
	st, err := fetchRemoteStatus(ctx, u.admin)
	return err == nil && st.Transport == remote.TransportTailscale && st.Backend == remote.BackendEmbedded
}

func (u *uninstaller) defaultsDir() string { return filepath.Join(u.cfg.dataDir, "defaults") }

func (u *uninstaller) tailscaleDir() string { return filepath.Join(u.cfg.dataDir, "tailscale") }

func (u *uninstaller) purge() {
	for _, p := range u.purgeFiles() {
		if fi, err := os.Lstat(p); err != nil || fi.IsDir() {
			continue
		}
		if err := os.Remove(p); err == nil {
			fmt.Fprintf(u.out, "Deleted %s\n", p)
		}
	}
	// The Tailscale state directory is deleted as a tree only when it holds
	// the files agentflow and tsnet create there, so a person's own
	// ~/tailscale folder survives AF_DB=$HOME/agentflow.db.
	if ts := u.tailscaleDir(); looksLikeTailscaleState(ts) {
		if err := os.RemoveAll(ts); err == nil {
			fmt.Fprintf(u.out, "Deleted %s\n", ts)
		}
	}
	if safeToRemoveTree(u.uploadsRoot, u.home) {
		if _, err := os.Stat(u.uploadsRoot); err == nil {
			if err := os.RemoveAll(u.uploadsRoot); err == nil {
				fmt.Fprintf(u.out, "Deleted %s\n", u.uploadsRoot)
			}
		}
	} else {
		fmt.Fprintf(u.out, "Left %s in place: it is not a directory agentflow can safely delete.\n", u.uploadsRoot)
	}
	// Directories are only removed when empty.
	for _, d := range []string{
		adminsock.Dir(u.cfg.dataDir),
		filepath.Dir(cloudflared.NewInstaller(u.cfg.dataDir).Path()),
		u.defaultsDir(),
		u.cfg.dataDir,
		filepath.Dir(u.envFile),
	} {
		if d == "" || d == "." || d == u.home || d == "/" {
			continue
		}
		if err := os.Remove(d); err == nil {
			fmt.Fprintf(u.out, "Removed empty directory %s\n", d)
		}
	}
}

func (u *uninstaller) leftovers() {
	fmt.Fprintln(u.out)
	fmt.Fprintln(u.out, "Not touched:")
	fmt.Fprintln(u.out, "  - project trust entries agentflow added to ~/.claude.json")
	fmt.Fprintln(u.out, "  - .agentflow-bak-* backups written next to Claude transcripts by `agentflow make-resumable`")
}

// looksLikeTailscaleState reports whether dir holds agentflow's Tailscale
// node state.
func looksLikeTailscaleState(dir string) bool {
	for _, marker := range []string{"tailscaled.state", "hostname"} {
		if fi, err := os.Stat(filepath.Join(dir, marker)); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

// safeToRemoveTree refuses paths that are relative, the filesystem root, the
// home directory or one of its ancestors.
func safeToRemoveTree(path, home string) bool {
	if path == "" || !filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(path)
	if clean == string(filepath.Separator) {
		return false
	}
	if home == "" {
		return false
	}
	rel, err := filepath.Rel(clean, filepath.Clean(home))
	if err == nil && (rel == "." || !strings.HasPrefix(rel, "..")) {
		// path is home or contains home
		return false
	}
	return true
}
