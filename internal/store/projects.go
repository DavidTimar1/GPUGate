package store

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CodingProjectSlug is the one project seeded into a fresh database and the one that can never
// be deleted: the default project, never displaced by a project made later.
const CodingProjectSlug = "coding"

// projectSlugPattern is the shape of a slug: 1 to 32 characters of lowercase ASCII letters,
// digits and dashes, starting with a letter. That is what /p/<slug>/... can route: the router
// reads the segment straight off the URL path, so anything this pattern excludes would either
// not round-trip through a URL cleanly or collide with another route.
var projectSlugPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidProjectSlug reports whether slug is a shape CreateProject may accept. The console handler
// checks it before a slug reaches the store (and refuses by name), and it is checked again here
// so no other caller can write one around that check.
func ValidProjectSlug(slug string) bool {
	return projectSlugPattern.MatchString(slug)
}

// ErrProjectExists is CreateProject's refusal for a slug already on file.
var ErrProjectExists = errors.New("a project with that slug already exists")

// seedCodingProject writes the 'coding' row into `project` if it is not there yet. It runs on
// every Open: cheap, and idempotent by its own ON CONFLICT.
func seedCodingProject(db *sql.DB) error {
	_, err := db.Exec(
		`INSERT INTO project (slug, name, created_at) VALUES (?, ?, ?) ON CONFLICT(slug) DO NOTHING`,
		CodingProjectSlug, CodingProjectSlug, time.Now().UTC().Format(TimeLayout),
	)
	return err
}

// Project is one row of `project`: a name a claim can be launched for, with its own fixed
// address `/p/<slug>/...`.
type Project struct {
	Slug      string
	Name      string
	CreatedAt string
	// AutoMove is the project's switch for moving a serving model to a cheaper machine by itself
	// (project.auto_move), off by default.
	AutoMove bool
}

// ListProjects answers every project, 'coding' first, then the rest by created_at. A client
// that takes the first listed project gets the default project, never a later one.
func (s *Store) ListProjects() ([]Project, error) {
	rows, err := s.db.Query(
		`SELECT slug, name, created_at, auto_move FROM project ORDER BY (slug != 'coding'), created_at`,
	)
	if err != nil {
		return nil, fmt.Errorf("read the projects: %w", err)
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		if err := rows.Scan(&p.Slug, &p.Name, &p.CreatedAt, &p.AutoMove); err != nil {
			return nil, fmt.Errorf("read a project row: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ProjectExists reports whether slug names a project on file. The fixed address's 404 and the
// launch path's refusal both read this before doing anything else.
func (s *Store) ProjectExists(slug string) (bool, error) {
	var exists int
	err := s.db.QueryRow("SELECT 1 FROM project WHERE slug = ?", slug).Scan(&exists)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("check whether project %s exists: %w", slug, err)
	}
	return true, nil
}

// CreateProject writes a new project row. slug is assumed already validated by the caller
// (ValidProjectSlug). This only guards the one thing SQL itself enforces, a slug already taken,
// answered as ErrProjectExists rather than a raw driver error a caller would have to parse.
func (s *Store) CreateProject(slug, name string) error {
	_, err := s.db.Exec(
		`INSERT INTO project (slug, name, created_at) VALUES (?, ?, ?)`,
		slug, name, time.Now().UTC().Format(TimeLayout),
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return ErrProjectExists
		}
		return fmt.Errorf("create project %s: %w", slug, err)
	}
	return nil
}

// DeleteProject removes slug's row from `project`. Whether slug may be deleted at all ('coding'
// never can) is the console handler's decision, made before this is called; this never touches
// machine_claim, so a deleted project's past claims keep their project name.
func (s *Store) DeleteProject(slug string) error {
	_, err := s.db.Exec("DELETE FROM project WHERE slug = ?", slug)
	if err != nil {
		return fmt.Errorf("delete project %s: %w", slug, err)
	}
	return nil
}
