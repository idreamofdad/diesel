package main

import (
	"context"
	"path/filepath"
	"testing"

	"diesel/internal/knowledge"
	"diesel/internal/settings"
	"diesel/internal/storage"

	"fyne.io/fyne/v2/test"
)

// newTestKnowledge spins up a knowledge service over a temp DB, seeded with a
// small graph, for the desktop dialog smoke tests. Mutation correctness lives
// in internal/knowledge/store_test.go; these tests only guard the Fyne wiring.
func newTestKnowledge(t *testing.T) *knowledge.Service {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "diesel.db"))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := knowledge.New(st, settings.AppSettings{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		svc.Stop()
		_ = st.Close()
	})
	ctx := context.Background()
	store := svc.Store()
	if _, err := store.CreateEntities(ctx, []knowledge.Entity{
		{Name: "Ada", Type: "person", Observations: []string{"likes math"}},
		{Name: "Mochi", Type: "cat"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateRelations(ctx, []knowledge.Relation{
		{From: "Ada", To: "Mochi", RelationType: "owns"},
	}); err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestShowKnowledgeDialog_Builds checks the dialog constructs and shows against
// a populated graph without panicking (nil derefs, bad layouts).
func TestShowKnowledgeDialog_Builds(t *testing.T) {
	svc := newTestKnowledge(t)
	win := test.NewWindow(nil)
	defer win.Close()

	showKnowledgeDialog(win, svc)

	if got := len(win.Canvas().Overlays().List()); got == 0 {
		t.Fatal("expected the knowledge dialog to be shown as an overlay")
	}
}

// TestEditForms_Build checks each in-place edit helper constructs and shows its
// form without panicking — e.g. the relation form's SetSelected of the current
// endpoint, and valid FormItems throughout.
func TestEditForms_Build(t *testing.T) {
	svc := newTestKnowledge(t)
	ctx := context.Background()
	store := svc.Store()
	win := test.NewWindow(nil)
	defer win.Close()

	before := len(win.Canvas().Overlays().List())
	editEntityForm(ctx, win, store, knowledge.Entity{Name: "Ada", Type: "person"}, func(string) {})
	editObservationForm(ctx, win, store, "Ada", "likes math", func() {})
	editRelationForm(ctx, win, store, "Ada",
		knowledge.Relation{From: "Ada", To: "Mochi", RelationType: "owns"},
		[]string{"Mochi"}, func() {})

	if got := len(win.Canvas().Overlays().List()); got <= before {
		t.Fatalf("expected edit forms to show overlays, got %d (was %d)", got, before)
	}
}
