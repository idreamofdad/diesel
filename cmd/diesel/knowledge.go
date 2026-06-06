package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"diesel/internal/knowledge"
	"diesel/internal/settings"
	"diesel/internal/util"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/widget"
)

// showKnowledgeDialog presents a manual editor for Diesel's knowledge graph —
// the same entities, observations, and relations the model edits through its
// MCP tools. The left pane lists entities; selecting one shows its
// observations and the relations touching it, each with add/delete controls.
// Every mutation writes straight through the knowledge store and reloads, so
// the next turn's system-prompt injection reflects the change.
func showKnowledgeDialog(win fyne.Window, knMgr *knowledge.Service) {
	store := knMgr.Store()
	ctx := context.Background()

	var (
		graph    knowledge.Graph
		selected string // currently-selected entity name, "" when none
	)

	// Forward declarations so the closures can refer to each other.
	var (
		reload       func()
		rebuildRight func()
	)

	entityList := widget.NewList(
		func() int { return len(graph.Entities) },
		func() fyne.CanvasObject { return widget.NewLabel("") },
		func(i widget.ListItemID, o fyne.CanvasObject) {
			e := graph.Entities[i]
			o.(*widget.Label).SetText(fmt.Sprintf("%s  (%s)", e.Name, e.Type))
		},
	)

	right := container.NewVBox()
	rightScroll := container.NewVScroll(right)

	// fail surfaces a store error without tearing the dialog down.
	fail := func(err error) {
		if err != nil {
			dialog.ShowError(err, win)
		}
	}

	reload = func() {
		g, err := store.ReadGraph(ctx)
		if err != nil {
			fail(err)
			return
		}
		graph = g
		entityList.Refresh()
		rebuildRight()
	}

	// entityByName finds the loaded entity, or nil.
	entityByName := func(name string) *knowledge.Entity {
		for i := range graph.Entities {
			if graph.Entities[i].Name == name {
				return &graph.Entities[i]
			}
		}
		return nil
	}

	rebuildRight = func() {
		right.Objects = nil
		e := entityByName(selected)
		if e == nil {
			right.Add(widget.NewLabelWithStyle("Select an entity, or add one.", fyne.TextAlignLeading, fyne.TextStyle{Italic: true}))
			right.Refresh()
			return
		}

		entEdit := widget.NewButton("✎", func() {
			editEntityForm(ctx, win, store, *e, func(newName string) {
				selected = newName
				reload()
			})
		})
		hdr := widget.NewLabelWithStyle(fmt.Sprintf("%s  (%s)", e.Name, e.Type), fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
		right.Add(container.NewBorder(nil, nil, nil, entEdit, hdr))

		// ── Observations ──
		right.Add(widget.NewSeparator())
		right.Add(widget.NewLabelWithStyle("Observations", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		if len(e.Observations) == 0 {
			right.Add(widget.NewLabel("(none)"))
		}
		for _, obs := range e.Observations {
			obs := obs
			edit := widget.NewButton("✎", func() {
				editObservationForm(ctx, win, store, e.Name, obs, reload)
			})
			del := widget.NewButton("✕", func() {
				fail(store.DeleteObservations(ctx, []knowledge.ObservationMutation{{EntityName: e.Name, Contents: []string{obs}}}))
				reload()
			})
			lbl := widget.NewLabel(obs)
			lbl.Wrapping = fyne.TextWrapWord
			right.Add(container.NewBorder(nil, nil, nil, container.NewHBox(edit, del), lbl))
		}
		obsEntry := widget.NewEntry()
		obsEntry.SetPlaceHolder("New observation…")
		addObs := widget.NewButton("Add", func() {
			text := strings.TrimSpace(obsEntry.Text)
			if text == "" {
				return
			}
			fail(store.AddObservations(ctx, []knowledge.ObservationMutation{{EntityName: e.Name, Contents: []string{text}}}))
			obsEntry.SetText("")
			reload()
		})
		right.Add(container.NewBorder(nil, nil, nil, addObs, obsEntry))

		// ── Relations touching this entity ──
		right.Add(widget.NewSeparator())
		right.Add(widget.NewLabelWithStyle("Relations", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		var touching []knowledge.Relation
		for _, r := range graph.Relations {
			if r.From == e.Name || r.To == e.Name {
				touching = append(touching, r)
			}
		}
		if len(touching) == 0 {
			right.Add(widget.NewLabel("(none)"))
		}
		others := otherEntityNames(graph, e.Name)
		for _, r := range touching {
			r := r
			edit := widget.NewButton("✎", func() {
				editRelationForm(ctx, win, store, e.Name, r, others, reload)
			})
			del := widget.NewButton("✕", func() {
				fail(store.DeleteRelations(ctx, []knowledge.Relation{r}))
				reload()
			})
			lbl := widget.NewLabel(fmt.Sprintf("%s  —%s→  %s", r.From, r.RelationType, r.To))
			lbl.Wrapping = fyne.TextWrapWord
			right.Add(container.NewBorder(nil, nil, nil, container.NewHBox(edit, del), lbl))
		}
		// Add relation: this entity --relType--> target (another entity).
		relType := widget.NewEntry()
		relType.SetPlaceHolder("relation (e.g. owns)")
		target := widget.NewSelect(others, nil)
		if len(others) > 0 {
			target.SetSelected(others[0])
		}
		addRel := widget.NewButton("Add", func() {
			rt := strings.TrimSpace(relType.Text)
			if rt == "" || target.Selected == "" {
				return
			}
			_, err := store.CreateRelations(ctx, []knowledge.Relation{{From: e.Name, To: target.Selected, RelationType: rt}})
			fail(err)
			relType.SetText("")
			reload()
		})
		right.Add(widget.NewLabel(e.Name + " —…→"))
		right.Add(container.NewBorder(nil, nil, nil, addRel, container.NewGridWithColumns(2, relType, target)))

		right.Refresh()
	}

	entityList.OnSelected = func(i widget.ListItemID) {
		if i >= 0 && i < len(graph.Entities) {
			selected = graph.Entities[i].Name
			rebuildRight()
		}
	}

	// ── Toolbar ──
	addEntityBtn := widget.NewButton("Add entity", func() {
		set := settings.Load()
		exName := strings.TrimSpace(util.FirstNonEmpty(set.FirstName, "Ada") + " " + util.FirstNonEmpty(set.LastName, "Lovelace"))
		name := widget.NewEntry()
		name.SetPlaceHolder("Name, e.g. " + exName)
		etype := widget.NewEntry()
		etype.SetPlaceHolder("Type, e.g. person")
		form := dialog.NewForm("Add entity", "Add", "Cancel",
			[]*widget.FormItem{
				widget.NewFormItem("Name", name),
				widget.NewFormItem("Type", etype),
			},
			func(ok bool) {
				if !ok {
					return
				}
				n := strings.TrimSpace(name.Text)
				if n == "" {
					return
				}
				_, err := store.CreateEntities(ctx, []knowledge.Entity{{Name: n, Type: strings.TrimSpace(etype.Text)}})
				fail(err)
				selected = n
				reload()
			}, win)
		form.Resize(fyne.NewSize(420, 200))
		form.Show()
	})
	delEntityBtn := widget.NewButton("Delete entity", func() {
		if selected == "" {
			return
		}
		name := selected
		dialog.ShowConfirm("Delete entity",
			fmt.Sprintf("Delete %q and all its observations and relations?", name),
			func(ok bool) {
				if !ok {
					return
				}
				fail(store.DeleteEntities(ctx, []string{name}))
				selected = ""
				reload()
			}, win)
	})
	refreshBtn := widget.NewButton("Refresh", func() { reload() })

	toolbar := container.NewHBox(addEntityBtn, delEntityBtn, refreshBtn)

	split := container.NewHSplit(
		container.NewBorder(widget.NewLabelWithStyle("Entities", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), nil, nil, nil, entityList),
		rightScroll,
	)
	split.Offset = 0.38

	content := container.NewBorder(toolbar, nil, nil, nil, split)
	reload()

	d := dialog.NewCustom("Knowledge graph", "Close", content, win)
	d.Resize(fyne.NewSize(720, 560))
	d.Show()
}

// otherEntityNames returns the sorted entity names excluding `exclude`, for the
// relation-target dropdown.
func otherEntityNames(g knowledge.Graph, exclude string) []string {
	var out []string
	for _, e := range g.Entities {
		if e.Name != exclude {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}

// editEntityForm opens a modal form to rename and/or retype an entity, writes
// the change through the store, and calls onDone with the new name on success.
// A blank name or an unchanged name+type is a no-op. Shared by the list dialog
// and (later) the graph view's panel.
func editEntityForm(ctx context.Context, win fyne.Window, store *knowledge.Store, e knowledge.Entity, onDone func(newName string)) {
	name := widget.NewEntry()
	name.SetText(e.Name)
	etype := widget.NewEntry()
	etype.SetText(e.Type)
	form := dialog.NewForm("Edit entity", "Save", "Cancel",
		[]*widget.FormItem{
			widget.NewFormItem("Name", name),
			widget.NewFormItem("Type", etype),
		},
		func(ok bool) {
			if !ok {
				return
			}
			newName := strings.TrimSpace(name.Text)
			newType := strings.TrimSpace(etype.Text)
			if newName == "" || (newName == e.Name && newType == e.Type) {
				return
			}
			if err := store.EditEntities(ctx, []knowledge.EntityEdit{{Name: e.Name, NewName: newName, NewType: newType}}); err != nil {
				dialog.ShowError(err, win)
				return
			}
			onDone(newName)
		}, win)
	form.Resize(fyne.NewSize(420, 200))
	form.Show()
}

// editObservationForm opens a modal form to rewrite one of an entity's
// observations in place. Blank or unchanged text is a no-op.
func editObservationForm(ctx context.Context, win fyne.Window, store *knowledge.Store, entityName, oldContent string, onDone func()) {
	entry := widget.NewEntry()
	entry.SetText(oldContent)
	form := dialog.NewForm("Edit observation", "Save", "Cancel",
		[]*widget.FormItem{
			widget.NewFormItem("Observation", entry),
		},
		func(ok bool) {
			if !ok {
				return
			}
			next := strings.TrimSpace(entry.Text)
			if next == "" || next == oldContent {
				return
			}
			if err := store.EditObservations(ctx, []knowledge.ObservationEdit{{EntityName: entityName, OldContent: oldContent, NewContent: next}}); err != nil {
				dialog.ShowError(err, win)
				return
			}
			onDone()
		}, win)
	form.Resize(fyne.NewSize(440, 180))
	form.Show()
}

// editRelationForm opens a modal form to edit a relation touching pivot,
// preserving its direction: only the relation type and the other endpoint can
// change (mirroring the web). others lists the selectable endpoints (every
// entity but pivot). An unchanged triple is a no-op.
func editRelationForm(ctx context.Context, win fyne.Window, store *knowledge.Store, pivot string, r knowledge.Relation, others []string, onDone func()) {
	outgoing := r.From == pivot
	other := r.To
	dir := "→ outgoing"
	if !outgoing {
		other = r.From
		dir = "← incoming"
	}
	relType := widget.NewEntry()
	relType.SetText(r.RelationType)
	target := widget.NewSelect(others, nil)
	target.SetSelected(other)
	form := dialog.NewForm("Edit relation", "Save", "Cancel",
		[]*widget.FormItem{
			widget.NewFormItem("Direction", widget.NewLabel(dir)),
			widget.NewFormItem("Relation", relType),
			widget.NewFormItem("Other", target),
		},
		func(ok bool) {
			if !ok {
				return
			}
			rt := strings.TrimSpace(relType.Text)
			sel := target.Selected
			if rt == "" || sel == "" {
				return
			}
			next := knowledge.Relation{From: pivot, To: sel, RelationType: rt}
			if !outgoing {
				next = knowledge.Relation{From: sel, To: pivot, RelationType: rt}
			}
			if next == r {
				return
			}
			if err := store.EditRelations(ctx, []knowledge.RelationEdit{{Old: r, New: next}}); err != nil {
				dialog.ShowError(err, win)
				return
			}
			onDone()
		}, win)
	form.Resize(fyne.NewSize(460, 240))
	form.Show()
}
