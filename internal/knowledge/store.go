package knowledge

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// Store is the data-access layer for the knowledge graph. It runs its own
// SQL against the shared SQLite pool handed over by storage.Store.SQLDB(),
// the same pattern the Matrix bridge uses to own its tables without the
// storage package having to know about them.
type Store struct {
	db *sql.DB
}

// NewStore wraps a shared *sql.DB. The kg_* tables are created by storage's
// goose migrations at Open time, so the pool is ready to use here.
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// ObservationMutation is a batch of free-text facts targeting one entity —
// the unit both add_observations and delete_observations operate on.
type ObservationMutation struct {
	EntityName string   `json:"entityName"`
	Contents   []string `json:"contents"`
}

// CreateEntities upserts each entity by name and merges its observations in.
// A name that already exists keeps its original type (upsert-merge, matching
// the reference memory server) and simply gains any new observations; the
// (entity_name, content) uniqueness makes re-adding a fact a no-op. Returns
// the affected entities reloaded with their merged observation sets so the
// caller can show the model the resulting state.
func (s *Store) CreateEntities(ctx context.Context, ents []Entity) ([]Entity, error) {
	if len(ents) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	names := make([]string, 0, len(ents))
	for _, e := range ents {
		name := strings.TrimSpace(e.Name)
		if name == "" {
			return nil, fmt.Errorf("knowledge: entity name must not be empty")
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kg_entities (name, type) VALUES (?, ?)
			 ON CONFLICT(name) DO NOTHING`,
			name, strings.TrimSpace(e.Type),
		); err != nil {
			return nil, fmt.Errorf("knowledge: create entity %q: %w", name, err)
		}
		for _, obs := range e.Observations {
			obs = strings.TrimSpace(obs)
			if obs == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO kg_observations (entity_name, content) VALUES (?, ?)
				 ON CONFLICT(entity_name, content) DO NOTHING`,
				name, obs,
			); err != nil {
				return nil, fmt.Errorf("knowledge: add observation to %q: %w", name, err)
			}
		}
		names = append(names, name)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("knowledge: commit: %w", err)
	}
	return s.entitiesByName(ctx, names)
}

// DeleteEntities removes each named entity. ON DELETE CASCADE takes its
// observations and any relation touching it with it, so the graph never
// keeps a dangling reference. Unknown names are silently ignored.
func (s *Store) DeleteEntities(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, name := range names {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM kg_entities WHERE name = ?`, strings.TrimSpace(name),
		); err != nil {
			return fmt.Errorf("knowledge: delete entity %q: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("knowledge: commit: %w", err)
	}
	return nil
}

// AddObservations appends facts to existing entities. The target entity must
// already exist — a missing one is reported as a clear error (rather than the
// raw foreign-key failure) so the model knows to create the entity first.
func (s *Store) AddObservations(ctx context.Context, muts []ObservationMutation) error {
	if len(muts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, m := range muts {
		name := strings.TrimSpace(m.EntityName)
		ok, err := entityExistsTx(ctx, tx, name)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("knowledge: entity %q does not exist; create it before adding observations", name)
		}
		for _, obs := range m.Contents {
			obs = strings.TrimSpace(obs)
			if obs == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO kg_observations (entity_name, content) VALUES (?, ?)
				 ON CONFLICT(entity_name, content) DO NOTHING`,
				name, obs,
			); err != nil {
				return fmt.Errorf("knowledge: add observation to %q: %w", name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("knowledge: commit: %w", err)
	}
	return nil
}

// DeleteObservations removes specific facts from entities. Missing entities
// or facts are no-ops — the end state (the fact is gone) is what matters.
func (s *Store) DeleteObservations(ctx context.Context, muts []ObservationMutation) error {
	if len(muts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, m := range muts {
		name := strings.TrimSpace(m.EntityName)
		for _, obs := range m.Contents {
			if _, err := tx.ExecContext(ctx,
				`DELETE FROM kg_observations WHERE entity_name = ? AND content = ?`,
				name, strings.TrimSpace(obs),
			); err != nil {
				return fmt.Errorf("knowledge: delete observation from %q: %w", name, err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("knowledge: commit: %w", err)
	}
	return nil
}

// ObservationEdit rewrites one existing fact on an entity from OldContent to
// NewContent — the unit the editor uses to change an observation in place
// rather than deleting and re-adding it.
type ObservationEdit struct {
	EntityName string `json:"entityName"`
	OldContent string `json:"oldContent"`
	NewContent string `json:"newContent"`
}

// EditObservations rewrites specific facts in place. Each edit changes one
// entity's observation from OldContent to NewContent, keeping the underlying
// row — and so the fact's position in the list — instead of appending a fresh
// one the way a delete-then-add would. Empty new text is rejected; an edit
// whose old text no longer matches is a silent no-op, mirroring
// DeleteObservations' leniency. If NewContent already exists on the entity the
// pre-existing copy is folded into the edited row so the (entity_name, content)
// uniqueness still holds.
func (s *Store) EditObservations(ctx context.Context, edits []ObservationEdit) error {
	if len(edits) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, e := range edits {
		name := strings.TrimSpace(e.EntityName)
		oldC := strings.TrimSpace(e.OldContent)
		newC := strings.TrimSpace(e.NewContent)
		if newC == "" {
			return fmt.Errorf("knowledge: observation must not be empty")
		}
		if oldC == newC {
			continue
		}
		// Drop any existing copy of the target text first so the in-place
		// UPDATE can't trip the (entity_name, content) uniqueness constraint.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM kg_observations WHERE entity_name = ? AND content = ?`,
			name, newC,
		); err != nil {
			return fmt.Errorf("knowledge: edit observation on %q: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE kg_observations SET content = ? WHERE entity_name = ? AND content = ?`,
			newC, name, oldC,
		); err != nil {
			return fmt.Errorf("knowledge: edit observation on %q: %w", name, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("knowledge: commit: %w", err)
	}
	return nil
}

// CreateRelations adds directed, typed edges. Both endpoints must already
// exist: a relation to a missing entity is rejected with a clear error so the
// graph stays consistent (no auto-created stub nodes). Re-adding the same
// triple is idempotent. Returns the relations that now exist for the input.
func (s *Store) CreateRelations(ctx context.Context, rels []Relation) ([]Relation, error) {
	if len(rels) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	out := make([]Relation, 0, len(rels))
	for _, r := range rels {
		from := strings.TrimSpace(r.From)
		to := strings.TrimSpace(r.To)
		rel := strings.TrimSpace(r.RelationType)
		if rel == "" {
			return nil, fmt.Errorf("knowledge: relation type must not be empty")
		}
		for label, name := range map[string]string{"from": from, "to": to} {
			ok, err := entityExistsTx(ctx, tx, name)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("knowledge: %s entity %q does not exist; create it before relating", label, name)
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO kg_relations (from_name, to_name, relation_type) VALUES (?, ?, ?)
			 ON CONFLICT(from_name, to_name, relation_type) DO NOTHING`,
			from, to, rel,
		); err != nil {
			return nil, fmt.Errorf("knowledge: create relation %q -%s-> %q: %w", from, rel, to, err)
		}
		out = append(out, Relation{From: from, To: to, RelationType: rel})
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("knowledge: commit: %w", err)
	}
	return out, nil
}

// DeleteRelations removes specific edges by exact triple. Missing edges are
// no-ops.
func (s *Store) DeleteRelations(ctx context.Context, rels []Relation) error {
	if len(rels) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("knowledge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, r := range rels {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM kg_relations WHERE from_name = ? AND to_name = ? AND relation_type = ?`,
			strings.TrimSpace(r.From), strings.TrimSpace(r.To), strings.TrimSpace(r.RelationType),
		); err != nil {
			return fmt.Errorf("knowledge: delete relation: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("knowledge: commit: %w", err)
	}
	return nil
}

// ReadGraph returns the entire graph. Both slices are non-nil even when empty
// so the serialized JSON reads as [] rather than null.
func (s *Store) ReadGraph(ctx context.Context) (Graph, error) {
	return s.graphFor(ctx, nil)
}

// SearchNodes returns the subgraph whose entities match query (case-
// insensitive substring against name, type, or any observation), together
// with the relations that run between matched entities — the same semantics
// as the reference memory server's search_nodes.
func (s *Store) SearchNodes(ctx context.Context, query string) (Graph, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return Graph{Entities: []Entity{}, Relations: []Relation{}}, nil
	}
	like := "%" + strings.ToLower(query) + "%"
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT e.name FROM kg_entities e
		 LEFT JOIN kg_observations o ON o.entity_name = e.name
		 WHERE lower(e.name) LIKE ? OR lower(e.type) LIKE ? OR lower(o.content) LIKE ?`,
		like, like, like,
	)
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: search: %w", err)
	}
	names, err := scanNames(rows)
	if err != nil {
		return Graph{}, err
	}
	return s.graphFor(ctx, names)
}

// OpenNodes returns the subgraph for an explicit set of entity names plus the
// relations among them.
func (s *Store) OpenNodes(ctx context.Context, names []string) (Graph, error) {
	trimmed := make([]string, 0, len(names))
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			trimmed = append(trimmed, n)
		}
	}
	if len(trimmed) == 0 {
		return Graph{Entities: []Entity{}, Relations: []Relation{}}, nil
	}
	return s.graphFor(ctx, trimmed)
}

// graphFor loads entities (all when names is nil, otherwise just the named
// set) with their observations, plus the relations among the loaded set.
func (s *Store) graphFor(ctx context.Context, names []string) (Graph, error) {
	g := Graph{Entities: []Entity{}, Relations: []Relation{}}

	// Restrict to the requested names, if any. A non-nil but empty set means
	// "nothing matched", so return the empty graph rather than everything.
	want := map[string]bool{}
	filtered := names != nil
	if filtered {
		if len(names) == 0 {
			return g, nil
		}
		for _, n := range names {
			want[n] = true
		}
	}

	entRows, err := s.db.QueryContext(ctx, `SELECT name, type FROM kg_entities ORDER BY name`)
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: read entities: %w", err)
	}
	idx := map[string]int{}
	func() {
		defer func() { _ = entRows.Close() }()
		for entRows.Next() {
			var e Entity
			if err = entRows.Scan(&e.Name, &e.Type); err != nil {
				return
			}
			if filtered && !want[e.Name] {
				continue
			}
			e.Observations = []string{}
			idx[e.Name] = len(g.Entities)
			g.Entities = append(g.Entities, e)
		}
		err = entRows.Err()
	}()
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: scan entities: %w", err)
	}

	obsRows, err := s.db.QueryContext(ctx,
		`SELECT entity_name, content FROM kg_observations ORDER BY id`)
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: read observations: %w", err)
	}
	func() {
		defer func() { _ = obsRows.Close() }()
		for obsRows.Next() {
			var name, content string
			if err = obsRows.Scan(&name, &content); err != nil {
				return
			}
			if i, ok := idx[name]; ok {
				g.Entities[i].Observations = append(g.Entities[i].Observations, content)
			}
		}
		err = obsRows.Err()
	}()
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: scan observations: %w", err)
	}

	relRows, err := s.db.QueryContext(ctx,
		`SELECT from_name, to_name, relation_type FROM kg_relations ORDER BY id`)
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: read relations: %w", err)
	}
	func() {
		defer func() { _ = relRows.Close() }()
		for relRows.Next() {
			var r Relation
			if err = relRows.Scan(&r.From, &r.To, &r.RelationType); err != nil {
				return
			}
			// A relation belongs to the subgraph only when both endpoints are
			// in the loaded set — for the full graph (idx holds everything)
			// that's always true.
			if _, ok := idx[r.From]; !ok {
				continue
			}
			if _, ok := idx[r.To]; !ok {
				continue
			}
			g.Relations = append(g.Relations, r)
		}
		err = relRows.Err()
	}()
	if err != nil {
		return Graph{}, fmt.Errorf("knowledge: scan relations: %w", err)
	}
	return g, nil
}

// entitiesByName reloads the named entities with their observations, in the
// stable name order graphFor uses, dropping any name that no longer exists.
func (s *Store) entitiesByName(ctx context.Context, names []string) ([]Entity, error) {
	g, err := s.graphFor(ctx, names)
	if err != nil {
		return nil, err
	}
	return g.Entities, nil
}

func entityExistsTx(ctx context.Context, tx *sql.Tx, name string) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM kg_entities WHERE name = ?`, name).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("knowledge: check entity %q: %w", name, err)
	}
	return true, nil
}

func scanNames(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("knowledge: scan name: %w", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("knowledge: scan names: %w", err)
	}
	sort.Strings(out)
	return out, nil
}
