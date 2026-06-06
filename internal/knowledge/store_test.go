package knowledge

import (
	"context"
	"path/filepath"
	"testing"

	"diesel/internal/storage"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTestStore opens a real SQLite database in a temp dir (running the goose
// migrations, including the kg_* tables, with foreign keys enabled via the DSN
// pragma) and returns a knowledge Store over its pool.
func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "diesel.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })
	return NewStore(st.SQLDB())
}

func TestCreateEntities_UpsertMergesObservations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr Mactire", Type: "person", Observations: []string{"works at McDonalds"}}})
	require.NoError(t, err)

	// Re-creating the same name with a different type keeps the original type
	// and merges the new observation in; the duplicate observation is ignored.
	got, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr Mactire", Type: "robot", Observations: []string{"works at McDonalds", "likes wolves"}}})
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "person", got[0].Type, "type is preserved on upsert-merge")
	assert.ElementsMatch(t, []string{"works at McDonalds", "likes wolves"}, got[0].Observations)
}

func TestDeleteEntities_CascadesObservationsAndRelations(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateEntities(ctx, []Entity{
		{Name: "Tyr Mactire", Type: "person", Observations: []string{"works at McDonalds"}},
		{Name: "Beckett", Type: "cat"},
	})
	require.NoError(t, err)
	_, err = s.CreateRelations(ctx, []Relation{{From: "Tyr Mactire", To: "Beckett", RelationType: "owns"}})
	require.NoError(t, err)

	require.NoError(t, s.DeleteEntities(ctx, []string{"Beckett"}))

	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	assert.Len(t, g.Entities, 1, "only Tyr remains")
	assert.Empty(t, g.Relations, "the owns relation cascaded away with Beckett")
}

func TestCreateRelations_RejectsDanglingEndpoint(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	_, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr Mactire", Type: "person"}})
	require.NoError(t, err)

	// Beckett was never created — the relation must be rejected, not stubbed.
	_, err = s.CreateRelations(ctx, []Relation{{From: "Tyr Mactire", To: "Beckett", RelationType: "owns"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Beckett")

	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	assert.Empty(t, g.Relations)
	assert.Len(t, g.Entities, 1, "no stub entity was created")
}

func TestAddObservations_RequiresExistingEntity(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	err := s.AddObservations(ctx, []ObservationMutation{{EntityName: "Ghost", Contents: []string{"boo"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ghost")
}

func TestDeleteObservations_RemovesFact(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr", Type: "person", Observations: []string{"a", "b"}}})
	require.NoError(t, err)
	require.NoError(t, s.DeleteObservations(ctx, []ObservationMutation{{EntityName: "Tyr", Contents: []string{"a"}}}))
	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	require.Len(t, g.Entities, 1)
	assert.Equal(t, []string{"b"}, g.Entities[0].Observations)
}

func TestEditObservations_RewritesInPlace(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr", Type: "person", Observations: []string{"a", "b", "c"}}})
	require.NoError(t, err)
	require.NoError(t, s.EditObservations(ctx, []ObservationEdit{
		{EntityName: "Tyr", OldContent: "b", NewContent: "B!"},
	}))
	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	require.Len(t, g.Entities, 1)
	// The edited fact keeps its original position rather than moving to the end.
	assert.Equal(t, []string{"a", "B!", "c"}, g.Entities[0].Observations)
}

func TestEditObservations_FoldsExistingDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr", Type: "person", Observations: []string{"a", "b"}}})
	require.NoError(t, err)
	// Editing "a" into "b" must not violate (entity_name, content) uniqueness;
	// the pre-existing "b" is folded into the edited row, leaving a single "b".
	require.NoError(t, s.EditObservations(ctx, []ObservationEdit{
		{EntityName: "Tyr", OldContent: "a", NewContent: "b"},
	}))
	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	require.Len(t, g.Entities, 1)
	assert.Equal(t, []string{"b"}, g.Entities[0].Observations)
}

func TestEditObservations_RejectsEmptyNewContent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "Tyr", Type: "person", Observations: []string{"a"}}})
	require.NoError(t, err)
	err = s.EditObservations(ctx, []ObservationEdit{{EntityName: "Tyr", OldContent: "a", NewContent: "  "}})
	require.Error(t, err)
}

func TestEditRelations_RetargetsAndRetypes(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "A", Type: "p"}, {Name: "B", Type: "p"}, {Name: "C", Type: "p"}})
	require.NoError(t, err)
	_, err = s.CreateRelations(ctx, []Relation{{From: "A", To: "B", RelationType: "owns"}})
	require.NoError(t, err)
	require.NoError(t, s.EditRelations(ctx, []RelationEdit{{
		Old: Relation{From: "A", To: "B", RelationType: "owns"},
		New: Relation{From: "A", To: "C", RelationType: "loves"},
	}}))
	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	require.Len(t, g.Relations, 1)
	assert.Equal(t, Relation{From: "A", To: "C", RelationType: "loves"}, g.Relations[0])
}

func TestEditRelations_RejectsDanglingNewEndpoint(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "A", Type: "p"}, {Name: "B", Type: "p"}})
	require.NoError(t, err)
	_, err = s.CreateRelations(ctx, []Relation{{From: "A", To: "B", RelationType: "owns"}})
	require.NoError(t, err)
	err = s.EditRelations(ctx, []RelationEdit{{
		Old: Relation{From: "A", To: "B", RelationType: "owns"},
		New: Relation{From: "A", To: "Ghost", RelationType: "owns"},
	}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Ghost")
}

func TestEditRelations_FoldsExistingDuplicate(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{{Name: "A", Type: "p"}, {Name: "B", Type: "p"}})
	require.NoError(t, err)
	_, err = s.CreateRelations(ctx, []Relation{
		{From: "A", To: "B", RelationType: "owns"},
		{From: "A", To: "B", RelationType: "likes"},
	})
	require.NoError(t, err)
	// Editing "owns" into the already-present "likes" must leave a single edge.
	require.NoError(t, s.EditRelations(ctx, []RelationEdit{{
		Old: Relation{From: "A", To: "B", RelationType: "owns"},
		New: Relation{From: "A", To: "B", RelationType: "likes"},
	}}))
	g, err := s.ReadGraph(ctx)
	require.NoError(t, err)
	require.Len(t, g.Relations, 1)
	assert.Equal(t, "likes", g.Relations[0].RelationType)
}

func TestSearchNodes_MatchesAcrossFields(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	_, err := s.CreateEntities(ctx, []Entity{
		{Name: "Tyr Mactire", Type: "person", Observations: []string{"works at McDonalds"}},
		{Name: "Beckett", Type: "cat"},
		{Name: "Diesel", Type: "person", Observations: []string{"owns an auto shop"}},
	})
	require.NoError(t, err)
	_, err = s.CreateRelations(ctx, []Relation{{From: "Tyr Mactire", To: "Beckett", RelationType: "owns"}})
	require.NoError(t, err)

	// Match by observation text.
	g, err := s.SearchNodes(ctx, "mcdonalds")
	require.NoError(t, err)
	require.Len(t, g.Entities, 1)
	assert.Equal(t, "Tyr Mactire", g.Entities[0].Name)
	// The owns relation isn't included because Beckett didn't match.
	assert.Empty(t, g.Relations)

	// Match by type returns both people but only relations between matched nodes.
	g, err = s.SearchNodes(ctx, "person")
	require.NoError(t, err)
	assert.Len(t, g.Entities, 2)
}

func TestReadGraph_EmptyIsNonNil(t *testing.T) {
	s := openTestStore(t)
	g, err := s.ReadGraph(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, g.Entities)
	assert.NotNil(t, g.Relations)
	assert.Empty(t, g.Entities)
}
