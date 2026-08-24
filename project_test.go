package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeProjects(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, projectsFileName)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTS_PROJECTS_FILE", path)
}

func baseConfig() Config {
	return Config{
		BoardID:           "board-default",
		IntegrationBranch: "dev",
		Repo: RepoConfig{
			URL: "git://host/default.git", Branch: "main", BranchPrefix: "agent/",
			Image: "golang:1.25", RunnerClass: "agent-dev", TestCommand: "go test ./...",
			TimeoutSecs: 900,
		},
	}
}

// A HOST CONFIGURED BEFORE PROJECTS EXISTED MUST KEEP WORKING. Its board and
// repository become one project, so nothing has to be migrated to upgrade.
func TestNoProjectsFileFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("AGENTS_PROJECTS_FILE", filepath.Join(t.TempDir(), "absent.json"))
	got, err := LoadProjects(baseConfig())
	if err != nil {
		t.Fatalf("LoadProjects() = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d projects, want the single one the environment describes", len(got))
	}
	if got[0].BoardID != "board-default" || got[0].Repo.URL != "git://host/default.git" {
		t.Errorf("the fallback project does not describe the configured host: %+v", got[0])
	}
	if !got[0].Active() {
		t.Error("the fallback project is disabled, so the host would run nothing")
	}
}

// A project names what differs and inherits the rest. Copying every repository
// setting into each entry would mean a host-wide change had to be made once per
// project, and the entries would drift.
func TestAProjectInheritsHostDefaults(t *testing.T) {
	writeProjects(t, `[{"name":"api","board_id":"b-api","repo":{"url":"git://host/api.git","branch":"trunk"}}]`)
	got, err := LoadProjects(baseConfig())
	if err != nil {
		t.Fatalf("LoadProjects() = %v", err)
	}
	p := got[0]
	if p.Repo.URL != "git://host/api.git" || p.Repo.Branch != "trunk" {
		t.Errorf("the project's own settings were lost: %+v", p.Repo)
	}
	if p.Repo.Image != "golang:1.25" || p.Repo.TestCommand != "go test ./..." {
		t.Errorf("host defaults were not inherited: %+v", p.Repo)
	}
	if p.Repo.BranchPrefix != "agent/" || p.Repo.TimeoutSecs != 900 {
		t.Errorf("host defaults were not inherited: %+v", p.Repo)
	}
	if p.IntegrationBranch != "dev" {
		t.Errorf("integration branch = %q, want the host default", p.IntegrationBranch)
	}
}

// TWO PROJECTS ON ONE BOARD is refused rather than merged: their dispatchers
// would both claim from the same columns and hand each other's tickets to the
// wrong repository, which looks like an agent building the wrong thing rather
// than a configuration mistake.
func TestTwoProjectsCannotShareABoard(t *testing.T) {
	writeProjects(t, `[{"name":"a","board_id":"same"},{"name":"b","board_id":"same"}]`)
	if _, err := LoadProjects(baseConfig()); err == nil {
		t.Fatal("two projects on one board were accepted")
	}
}

func TestProjectsMustBeNamedAndBoarded(t *testing.T) {
	writeProjects(t, `[{"name":"","board_id":"b1"}]`)
	if _, err := LoadProjects(baseConfig()); err == nil {
		t.Error("a nameless project was accepted")
	}
	writeProjects(t, `[{"name":"a","board_id":""}]`)
	if _, err := LoadProjects(baseConfig()); err == nil {
		t.Error("a project with no board was accepted; it would have nowhere to take work from")
	}
	writeProjects(t, `[{"name":"a","board_id":"b1"},{"name":"a","board_id":"b2"}]`)
	if _, err := LoadProjects(baseConfig()); err == nil {
		t.Error("two projects with one name were accepted")
	}
}

// Absent means enabled: the common case is a project you want running, and
// requiring "enabled": true on every entry would make the file boilerplate.
func TestAProjectCanBePausedWithoutBeingDeleted(t *testing.T) {
	no, yes := false, true
	if (Project{}).Active() != true {
		t.Error("a project with no enabled flag is treated as disabled")
	}
	if (Project{Enabled: &no}).Active() {
		t.Error("an explicitly disabled project is active")
	}
	if !(Project{Enabled: &yes}).Active() {
		t.Error("an explicitly enabled project is inactive")
	}

	writeProjects(t, `[{"name":"a","board_id":"b1","enabled":false},{"name":"b","board_id":"b2"}]`)
	got, err := LoadProjects(baseConfig())
	if err != nil {
		t.Fatalf("LoadProjects() = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("a paused project was dropped from the list; it must stay visible to be resumed")
	}
	if got[0].Active() || !got[1].Active() {
		t.Errorf("enabled flags were not honoured: %+v", got)
	}
}

// A malformed file is reported rather than silently becoming "no projects",
// which would look like a department with nothing to do.
func TestAMalformedProjectsFileIsAnError(t *testing.T) {
	writeProjects(t, `{"not":"a list"}`)
	if _, err := LoadProjects(baseConfig()); err == nil {
		t.Error("a malformed projects file was accepted")
	}
}

// An empty list falls back rather than running nothing: an empty file is far
// more likely to be a mistake than a deliberate instruction to idle.
func TestAnEmptyProjectListFallsBack(t *testing.T) {
	writeProjects(t, `[]`)
	got, err := LoadProjects(baseConfig())
	if err != nil || len(got) != 1 || got[0].BoardID != "board-default" {
		t.Errorf("an empty list did not fall back to the environment: %+v %v", got, err)
	}
}

// What is written must always be readable back — otherwise an edit made in the
// window bricks the next restart.
func TestSavedProjectsRoundTrip(t *testing.T) {
	writeProjects(t, `[]`)
	no := false
	want := []Project{
		{Name: "api", BoardID: "b-api", Repo: RepoConfig{URL: "git://host/api.git", Branch: "trunk"}},
		{Name: "web", BoardID: "b-web", Repo: RepoConfig{URL: "git://host/web.git"}, Enabled: &no},
	}
	if err := SaveProjects(want); err != nil {
		t.Fatalf("SaveProjects() = %v", err)
	}
	got, err := LoadProjects(baseConfig())
	if err != nil {
		t.Fatalf("LoadProjects() = %v", err)
	}
	if len(got) != 2 || got[0].Name != "api" || got[1].Name != "web" {
		t.Fatalf("round trip lost projects: %+v", got)
	}
	if got[0].Repo.Branch != "trunk" {
		t.Errorf("a project's own setting was lost: %+v", got[0].Repo)
	}
	if got[1].Active() {
		t.Error("the paused project came back enabled")
	}
	// Inheritance still applies on the way back in.
	if got[1].Repo.Image != "golang:1.25" {
		t.Errorf("host defaults were not inherited after a save: %+v", got[1].Repo)
	}
}

// A bad edit is refused BEFORE the write, so the running configuration survives
// it. Writing first and reporting afterwards would mean the next restart fails
// on a file the person thought they had saved.
func TestSaveRefusesAListThatCannotBeLoaded(t *testing.T) {
	writeProjects(t, `[{"name":"keep","board_id":"b-keep"}]`)
	before, err := os.ReadFile(projectsPath())
	if err != nil {
		t.Fatal(err)
	}

	for name, bad := range map[string][]Project{
		"no projects":    {},
		"nameless":       {{Name: "", BoardID: "b1"}},
		"boardless":      {{Name: "a", BoardID: ""}},
		"duplicate name": {{Name: "a", BoardID: "b1"}, {Name: "a", BoardID: "b2"}},
		"shared board":   {{Name: "a", BoardID: "b1"}, {Name: "b", BoardID: "b1"}},
	} {
		if err := SaveProjects(bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}

	after, err := os.ReadFile(projectsPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Error("a refused save still modified the file; the running configuration was damaged")
	}
}

// The file names repositories and, through them, what this host holds
// credentials for.
func TestSavedProjectsFileIsPrivate(t *testing.T) {
	writeProjects(t, `[]`)
	if err := SaveProjects([]Project{{Name: "a", BoardID: "b1"}}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(projectsPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("projects file mode = %o, want 600", perm)
	}
}
