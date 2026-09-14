// defaults.go — the shipped plays and rules, parsed from embedded yaml.
//
// The EMBEDDED bytes are authoritative and are never read from disk, so a
// deleted or corrupt file on someone's machine physically cannot stop the
// daemon. A generated copy is written to the data directory on every boot for
// reading and diffing; nothing ever reads it back.
//
// Yaml rather than JSON because the entire content is multi-line prose: a JSON
// catalogue makes every brief one line of escapes, which is unreviewable in a
// diff and unwritable by hand. That is the first direct dependency in
// go.mod, gopkg.in/yaml.v3, and it is worth it for that reason alone.

package store

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

//go:embed defaults/*.yml
var defaultsFS embed.FS

// playsFile and rulesFile are the two documents' shapes. They map one to one
// onto the structs the engine already uses, so this is yaml tags on what
// exists rather than a second model.
type playsFile struct {
	Plays []Play `yaml:"plays"`
}

type rulesFile struct {
	// Roles is the one-line statement of what each role is for. It used to be
	// a map literal in prompt.go, which made a fourth place to look for prose
	// after briefs and rules moved here.
	Roles        map[string]string `yaml:"roles"`
	Shared       []Rule            `yaml:"shared"`
	Worker       []Rule            `yaml:"worker"`
	Orchestrator []Rule            `yaml:"orchestrator"`
	// SoloRules are rules only a session with no loop uses. Kept out of
	// shared so a loop prompt never carries two reporting contracts.
	SoloRules []Rule    `yaml:"solorules"`
	Solo      SoloBlock `yaml:"solo"`
}

// SoloBlock is the prompt for a session with no play and no role. It
// references rules BY NAME from the same dictionary rather than copying text,
// so a rule edited once is edited everywhere.
type SoloBlock struct {
	Intro string   `yaml:"intro"`
	Rules []string `yaml:"rules"`
}

// The parsed defaults. These are the package's catalogue: everything that used
// to be a Go literal now arrives here.
var (
	plays             []Play
	roleJobs          map[string]string
	sharedRules       []Rule
	workerRules       []Rule
	orchestratorRules []Rule
	soloRules         []Rule
	soloBlock         SoloBlock
)

func init() {
	if err := loadEmbeddedDefaults(); err != nil {
		// A build bug, not a machine problem: these bytes are compiled in, so
		// if they are wrong here they are wrong everywhere, and starting with
		// a broken catalogue would hand agents briefs nobody wrote.
		panic("store: embedded defaults are invalid, this binary must not run: " + err.Error())
	}
}

func loadEmbeddedDefaults() error {
	praw, err := defaultsFS.ReadFile("defaults/plays.yml")
	if err != nil {
		return fmt.Errorf("read plays.yml: %w", err)
	}
	rraw, err := defaultsFS.ReadFile("defaults/rules.yml")
	if err != nil {
		return fmt.Errorf("read rules.yml: %w", err)
	}
	return applyDefaults(praw, rraw)
}

// applyDefaults parses and validates a pair of documents and installs them as
// the catalogue. It takes bytes rather than reading the embed so the refusals
// below can be exercised: a check with no way to reach it is a check nothing
// holds us to.
func applyDefaults(praw, rraw []byte) error {
	var pf playsFile
	if err := yaml.Unmarshal(praw, &pf); err != nil {
		return fmt.Errorf("parse plays.yml: %w", err)
	}
	var rf rulesFile
	if err := yaml.Unmarshal(rraw, &rf); err != nil {
		return fmt.Errorf("parse rules.yml: %w", err)
	}
	if len(pf.Plays) == 0 {
		return fmt.Errorf("plays.yml ships no plays")
	}
	for i := range pf.Plays {
		if err := pf.Plays[i].Validate(); err != nil {
			return fmt.Errorf("plays.yml: %w", err)
		}
	}
	if len(rf.Shared) == 0 {
		return fmt.Errorf("rules.yml ships no shared rules")
	}
	if len(rf.Solo.Rules) == 0 {
		return fmt.Errorf("rules.yml ships no solo rules")
	}
	plays = pf.Plays
	roleJobs = rf.Roles
	sharedRules = rf.Shared
	workerRules = rf.Worker
	orchestratorRules = rf.Orchestrator
	soloRules = rf.SoloRules
	soloBlock = rf.Solo
	// Every rule the solo block names must exist, or a solo prompt would
	// silently ship with a gap where a rule should be.
	for _, name := range soloBlock.Rules {
		if _, ok := LookupRule(name); !ok {
			return fmt.Errorf("rules.yml: solo block names %q, which is in no group", name)
		}
	}
	return nil
}

// DefaultsFileNames are the documents shipped in the binary.
var DefaultsFileNames = []string{"plays.yml", "rules.yml"}

// generatedHeader is prepended to the data-directory copy. It says plainly
// that the copy is output rather than input, because a file that looks
// editable and is not is worse than no file at all.
const generatedHeader = `# GENERATED - DO NOT EDIT.
#
# This is a copy of what the running binary uses, rewritten on every boot so it
# can never drift from the real thing. Editing it changes nothing.
#
# To change a brief or a rule, use the override surface: overrides are scoped
# to this machine, editable from any paired device, validated before they are
# stored, and they survive an upgrade of these defaults.
#
`

// WriteDefaultsCopy writes the shipped documents into dir for reading and
// diffing. Best effort by design: a copy that cannot be written is not a
// reason to refuse to run, because nothing reads it back.
func WriteDefaultsCopy(dir string) error {
	if dir == "" {
		return nil
	}
	out := filepath.Join(dir, "defaults")
	if err := os.MkdirAll(out, 0o750); err != nil {
		return err
	}
	for _, name := range DefaultsFileNames {
		raw, err := defaultsFS.ReadFile("defaults/" + name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(out, name), append([]byte(generatedHeader), raw...), 0o600); err != nil {
			return err
		}
	}
	return nil
}
