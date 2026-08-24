package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A PROJECT IS A BOARD PLUS A REPOSITORY, and the department runs several at
// once.
//
// Everything here used to be one board and one repo read from the environment,
// which made the host single-purpose: to work on something else you edited the
// config, restarted, and cleared the board first because the old run's tickets
// were still on it. That is fine for proving the pipeline works and useless for
// actually using it, since real work arrives for more than one codebase and the
// department is meant to be left running.
//
// The unit is deliberately the PAIR. A board without a repository has nowhere to
// put the work, and a repository without a board has nothing to say what the work
// is — and the claim protocol, the branch names and the integration branch are
// all scoped to one repository, so two projects sharing either would interleave
// in ways nothing downstream could untangle.
type Project struct {
	// Name is how a person refers to it: shown in the window, in logs, and in
	// nothing the machinery keys on. Ids are for that.
	Name string `json:"name"`
	// BoardID is the board this project's tickets live on. Separate boards are
	// what remove the need to clear between runs.
	BoardID string `json:"board_id"`
	// Repo is the repository its agents work. Fields left empty inherit the
	// host-wide defaults, so a project usually names only its URL and branch.
	Repo RepoConfig `json:"repo"`
	// IntegrationBranch is where reviewed work lands for this project. Empty
	// inherits the host default.
	IntegrationBranch string `json:"integration_branch,omitempty"`
	// Enabled allows a project to be switched off without deleting it, which is
	// what you want for something paused rather than finished.
	Enabled *bool `json:"enabled,omitempty"`
	// Auto turns on upkeep for this project: when its board has nothing on it,
	// the pipeline gives itself linter work rather than sitting still.
	//
	// PER PROJECT, because the trade differs per project. It spends the one
	// serving slot on work nobody asked for, which is free on a repository nobody
	// is waiting on and a delay on the one somebody is. A pointer so absent means
	// "whatever the host says" rather than "off" — see AutoMode.
	Auto *bool `json:"auto,omitempty"`
}

// Active reports whether this project should be dispatched. Absent means yes:
// the common case is a project you want running, and requiring "enabled": true
// on every entry would make the file mostly boilerplate.
func (p Project) Active() bool { return p.Enabled == nil || *p.Enabled }

// AutoMode reports whether this project fills its idle time with upkeep.
//
// Absent falls back to the host-wide setting, which is the same shape as every
// other field here: a host sets the default and a project disagrees when it has
// reason to. Unlike Active, absent does NOT mean yes — upkeep is off unless
// something says otherwise, because it spends a slot nobody asked it to.
func (p Project) AutoMode() bool {
	if p.Auto != nil {
		return *p.Auto
	}
	return upkeepEnabled()
}

// projectsFileName is the file read from the same directory as the env config,
// so both halves of a host's configuration live in one place.
const projectsFileName = "projects.json"

// LoadProjects reads the project list, falling back to the single project the
// environment describes.
//
// THE FALLBACK IS THE COMPATIBILITY STORY. A host configured before projects
// existed has AGENTS_BOARD_ID and AGENTS_REPO_URL and no file, and it must keep
// working untouched — so that pair becomes one project called "default". A file
// replaces that entirely rather than adding to it, because a host with both
// would have one project defined in two places and no obvious winner.
func LoadProjects(cfg Config) ([]Project, error) {
	path := projectsPath()
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return []Project{fromEnv(cfg)}, nil
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	var raw []Project
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(raw) == 0 {
		return []Project{fromEnv(cfg)}, nil
	}

	out := make([]Project, 0, len(raw))
	seenName := map[string]bool{}
	seenBoard := map[string]bool{}
	for i, p := range raw {
		p = p.inherit(cfg)
		if p.Name == "" {
			return nil, fmt.Errorf("%s: project %d has no name", path, i)
		}
		if p.BoardID == "" {
			return nil, fmt.Errorf("%s: project %q has no board_id", path, p.Name)
		}
		if seenName[p.Name] {
			return nil, fmt.Errorf("%s: two projects named %q", path, p.Name)
		}
		// TWO PROJECTS ON ONE BOARD is refused rather than merged. Their
		// dispatchers would both claim from the same columns and hand each other's
		// tickets to the wrong repository, and the failure would look like an
		// agent building the wrong thing rather than a configuration mistake.
		if seenBoard[p.BoardID] {
			return nil, fmt.Errorf("%s: project %q shares a board with another project", path, p.Name)
		}
		seenName[p.Name], seenBoard[p.BoardID] = true, true
		out = append(out, p)
	}
	return out, nil
}

// projectsPath is the projects file beside the env config.
func projectsPath() string {
	if p := strings.TrimSpace(os.Getenv("AGENTS_PROJECTS_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return projectsFileName
	}
	return filepath.Join(home, ".config", "codearmory-agents", projectsFileName)
}

// fromEnv turns the host-wide configuration into the single project it describes.
func fromEnv(cfg Config) Project {
	return Project{
		Name:              "default",
		BoardID:           cfg.BoardID,
		Repo:              cfg.Repo,
		IntegrationBranch: cfg.IntegrationBranch,
	}
}

// inherit fills a project's blanks from the host-wide configuration.
//
// Inheritance rather than repetition: a project normally differs from the host
// default in its name, its board and its clone URL, and everything else — the
// image, the runner class, the test command, the lease settings — is a property
// of how this HOST runs agents rather than of the project. Copying all of it into
// every entry would mean a host-wide change had to be made once per project, and
// the entries would drift.
func (p Project) inherit(cfg Config) Project {
	base := cfg.Repo
	r := p.Repo
	if r.URL == "" {
		r.URL = base.URL
	}
	if r.SecretRef == "" {
		r.SecretRef = base.SecretRef
	}
	if r.Branch == "" {
		r.Branch = base.Branch
	}
	if r.BranchPrefix == "" {
		r.BranchPrefix = base.BranchPrefix
	}
	if r.Image == "" {
		r.Image = base.Image
	}
	if r.RunnerClass == "" {
		r.RunnerClass = base.RunnerClass
	}
	if r.PipelineID == "" {
		r.PipelineID = base.PipelineID
	}
	if r.FormatCommand == "" {
		r.FormatCommand = base.FormatCommand
	}
	if r.DepsCommand == "" {
		r.DepsCommand = base.DepsCommand
	}
	if r.LintCommand == "" {
		r.LintCommand = base.LintCommand
	}
	if r.CriticalCommand == "" {
		r.CriticalCommand = base.CriticalCommand
	}
	if r.SCACommand == "" {
		r.SCACommand = base.SCACommand
	}
	if r.TestCommand == "" {
		r.TestCommand = base.TestCommand
	}
	if r.BranchInput == "" {
		r.BranchInput = base.BranchInput
	}
	if r.TimeoutSecs == 0 {
		r.TimeoutSecs = base.TimeoutSecs
	}
	if r.LeaseIdleSecs == 0 {
		r.LeaseIdleSecs = base.LeaseIdleSecs
	}
	if r.LeaseMaxSecs == 0 {
		r.LeaseMaxSecs = base.LeaseMaxSecs
	}
	p.Repo = r

	if p.IntegrationBranch == "" {
		p.IntegrationBranch = cfg.IntegrationBranch
	}
	return p
}

// SaveProjects writes the project list back.
//
// WRITTEN ATOMICALLY, because this file is the department's definition of what
// it works on and a half-written one is a host that starts with some projects
// missing. A temp file and a rename means a reader sees the old list or the new
// one, never a truncated one — and the service reads this at startup, which is
// exactly when a crash mid-write would be discovered.
//
// The file is 0600 for the same reason the env config is: it names repositories
// and, through them, what this host has credentials for.
func SaveProjects(ps []Project) error {
	if err := validateProjects(ps); err != nil {
		return err
	}
	path := projectsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	body, err := json.MarshalIndent(ps, "", "  ")
	if err != nil {
		return fmt.Errorf("encode projects: %w", err)
	}
	body = append(body, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(path), ".projects-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename succeeds
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("secure temp file: %w", err)
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("write projects: %w", err)
	}
	// Flushed before the rename: a rename is atomic in the directory, but it does
	// not promise the CONTENTS reached the disk, and a host that loses power here
	// would otherwise come back to an empty file that looks valid.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("flush projects: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// validateProjects applies the same rules as loading, so a list that is written
// can always be read back.
//
// Checked BEFORE the write rather than after: refusing a bad edit leaves the
// running configuration intact, whereas writing it and reporting the problem
// afterwards means the next restart fails on a file the person thought they had
// saved successfully.
func validateProjects(ps []Project) error {
	if len(ps) == 0 {
		return errors.New("a department needs at least one project")
	}
	seenName, seenBoard := map[string]bool{}, map[string]bool{}
	for i, p := range ps {
		name := strings.TrimSpace(p.Name)
		if name == "" {
			return fmt.Errorf("project %d has no name", i+1)
		}
		if strings.TrimSpace(p.BoardID) == "" {
			return fmt.Errorf("project %q has no board", name)
		}
		if seenName[name] {
			return fmt.Errorf("two projects are named %q", name)
		}
		if seenBoard[p.BoardID] {
			return fmt.Errorf("project %q shares a board with another project", name)
		}
		seenName[name], seenBoard[p.BoardID] = true, true
	}
	return nil
}
