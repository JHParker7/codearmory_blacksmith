// Package roles makes an agent's ROLE editable data instead of compiled-in code.
//
// A role is everything that makes one stage different from another — its prompt,
// what it may write (the guard), which tools it is offered, the command that
// decides it succeeded, its budgets. Those used to live as one hardcoded
// constructor per role in internal/agents; now they live as rows in postgres,
// so an operator edits them in the portal and a workflow picks one by name via a
// variable on the single `blacksmith/agent` action.
//
// This package is DATA ONLY: it depends on internal/tools (to rebuild a Guard
// from its serialized form) and nothing else of blacksmith's. The mapping from a
// stored Role to an agents.Options lives in the caller (package main), so that
// agents never has to import a package that pulls in gorm.
package roles

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	gormlogger "gorm.io/gorm/logger"

	"github.com/code-armory-app/blacksmith/internal/tools"
)

// Role is one stored role. The fields mirror agents.Options one-for-one, plus a
// couple that Options expresses as a Go type the database cannot hold directly:
// the guard is a discriminated GuardConfig (Options.Guard is a func), and the
// attempt timeout is seconds (Options.AttemptTimeout is a time.Duration).
type Role struct {
	Name       string       `json:"name"        gorm:"column:name;primaryKey"`
	Prompt     string       `json:"prompt"      gorm:"column:prompt"`
	Class      string       `json:"class"       gorm:"column:class"`
	Tools      []string     `json:"tools"       gorm:"column:tools;serializer:json"`
	Guard      *GuardConfig `json:"guard"       gorm:"column:guard;serializer:json"`
	TicketKind string       `json:"ticket_kind" gorm:"column:ticket_kind"`
	Check      string       `json:"check"       gorm:"column:check_cmd"`

	RewriteWhole       bool `json:"rewrite_whole"        gorm:"column:rewrite_whole"`
	EditInPlace        bool   `json:"edit_in_place"        gorm:"column:edit_in_place"`
	ReasoningEffort    string `json:"reasoning_effort"     gorm:"column:reasoning_effort"`
	AttemptTimeoutSecs int  `json:"attempt_timeout_secs" gorm:"column:attempt_timeout_secs"`
	Respins            int  `json:"respins"              gorm:"column:respins"`
	OwnCheck           bool `json:"own_check"            gorm:"column:own_check"`
	SeedKnown          bool `json:"seed_known"           gorm:"column:seed_known"`

	MaxIterations int     `json:"max_iterations" gorm:"column:max_iterations"`
	Temperature   float64 `json:"temperature"    gorm:"column:temperature"`
	MaxTokens     int     `json:"max_tokens"     gorm:"column:max_tokens"`

	// Thinking controls the model's reasoning scratchpad, and is ON BY DEFAULT
	// (NULL / nil). A role sets it false only to turn reasoning off for that role.
	// Default-on matters: with reasoning off the coding roles ship code that does
	// not compile (measured: a Store with `cannot assign to struct field in map`
	// and missing imports) and loop discovering the errors one at a time — the
	// 3-minute-dev-becomes-8-minute-timeout regression. A pointer so NULL (unset)
	// reads as the default rather than as an explicit "off", which a plain bool
	// column could not distinguish. The mapped request sends "high" for on (which
	// overrides any class-level AGENTS_LARGE_REASONING_EFFORT) and "none" for off.
	Thinking *bool `json:"thinking" gorm:"column:thinking"`
}

// TableName is explicit so the table is `roles`, not gorm's pluralized default of
// the struct name — the name a human reads in psql should be the obvious one.
func (Role) TableName() string { return "roles" }

// GuardConfig is a guard in serializable form: a discriminated union keyed on
// Kind. It rebuilds exactly the guards internal/tools/guard.go constructs —
// nothing more, because a guard the code cannot build is a role that cannot run.
type GuardConfig struct {
	Kind  string       `json:"kind"`            // allow_all|deny_all|no_tests|only_ext|only_basenames|both
	Exts  []string     `json:"exts,omitempty"`  // only_ext
	Names []string     `json:"names,omitempty"` // only_basenames
	A     *GuardConfig `json:"a,omitempty"`     // both
	B     *GuardConfig `json:"b,omitempty"`     // both
}

// Guard kinds.
const (
	GuardAllowAll      = "allow_all"
	GuardDenyAll       = "deny_all"
	GuardNoTests       = "no_tests"
	GuardOnlyExt       = "only_ext"
	GuardOnlyBasenames = "only_basenames"
	GuardBoth          = "both"
)

// Build turns a stored guard into the runtime tools.Guard. A nil config, or an
// unknown kind, denies every write — the same safe default agents.New applies
// when a role arrives with no guard at all, so a corrupt or half-written row
// fails closed rather than granting an agent free rein over the tree.
func (g *GuardConfig) Build() tools.Guard {
	if g == nil {
		return tools.DenyAll
	}
	switch g.Kind {
	case GuardAllowAll:
		return tools.AllowAll
	case GuardDenyAll:
		return tools.DenyAll
	case GuardNoTests:
		return tools.NoTests
	case GuardOnlyExt:
		return tools.OnlyExt(g.Exts...)
	case GuardOnlyBasenames:
		return tools.OnlyBasenames(g.Names...)
	case GuardBoth:
		return tools.Both(g.A.Build(), g.B.Build())
	default:
		return tools.DenyAll
	}
}

// Store is the postgres-backed role store. Its zero value is not usable; build
// one with Open.
type Store struct {
	db *gorm.DB
}

var (
	gormDB   *gorm.DB
	gormDBMu sync.Mutex
)

// connect opens (once) the shared gorm handle, mirroring how every CodeArmory
// service connects: a lazily-initialized singleton reading a postgres DSN. A
// Silent logger, because the agent's own logs are the ones an operator watches.
func connect(dsn string) (*gorm.DB, error) {
	gormDBMu.Lock()
	defer gormDBMu.Unlock()
	if gormDB != nil {
		return gormDB, nil
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("roles: connect: %w", err)
	}
	gormDB = db
	return db, nil
}

// Open connects, creates the table if absent, and seeds the default roles. The
// seed is idempotent and NON-destructive: a role an operator has edited is left
// exactly as they left it (OnConflict DoNothing), so a restart never reverts
// their changes — the defaults exist only to make a fresh database usable.
func Open(ctx context.Context, dsn string) (*Store, error) {
	if dsn == "" {
		return nil, errors.New("roles: a database is required; set AGENTS_DATABASE_URL")
	}
	db, err := connect(dsn)
	if err != nil {
		return nil, err
	}
	if err := db.WithContext(ctx).AutoMigrate(&Role{}); err != nil {
		return nil, fmt.Errorf("roles: migrate: %w", err)
	}
	s := &Store{db: db}
	if err := s.seed(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) seed(ctx context.Context) error {
	defs := Defaults()
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "name"}}, DoNothing: true}).
		Create(&defs).Error
}

// List returns every role, ordered by name so the portal and the CLI show a
// stable list.
func (s *Store) List(ctx context.Context) ([]Role, error) {
	var out []Role
	err := s.db.WithContext(ctx).Order("name").Find(&out).Error
	return out, err
}

// Get returns one role by name. A missing role is ErrNotFound, so a caller can
// tell "no such role" (a workflow naming a role that was deleted) from a database
// that is down.
func (s *Store) Get(ctx context.Context, name string) (Role, error) {
	var r Role
	err := s.db.WithContext(ctx).First(&r, "name = ?", name).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Role{}, ErrNotFound
	}
	return r, err
}

// Put upserts a role: create it or replace every column of an existing one. This
// is what the portal's Save and the CLI's edit both call.
func (s *Store) Put(ctx context.Context, r Role) error {
	if r.Name == "" {
		return errors.New("roles: a role needs a name")
	}
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "name"}}, UpdateAll: true}).
		Create(&r).Error
}

// Delete removes a role. Deleting a role a workflow still references does not
// error here — it surfaces later as ErrNotFound when that workflow runs, which
// is the right place for it: the workflow names the role that is gone.
func (s *Store) Delete(ctx context.Context, name string) error {
	return s.db.WithContext(ctx).Delete(&Role{}, "name = ?", name).Error
}

// ErrNotFound is returned by Get for a name that is not in the store.
var ErrNotFound = errors.New("roles: no such role")
