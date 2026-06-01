package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"diesel/internal/knowledge"

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

		right.Add(widget.NewLabelWithStyle(fmt.Sprintf("%s  (%s)", e.Name, e.Type), fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))

		// ── Observations ──
		right.Add(widget.NewSeparator())
		right.Add(widget.NewLabelWithStyle("Observations", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}))
		if len(e.Observations) == 0 {
			right.Add(widget.NewLabel("(none)"))
		}
		for _, obs := range e.Observations {
			obs := obs
			del := widget.NewButton("✕", func() {
				fail(store.DeleteObservations(ctx, []knowledge.ObservationMutation{{EntityName: e.Name, Contents: []string{obs}}}))
				reload()
			})
			lbl := widget.NewLabel(obs)
			lbl.Wrapping = fyne.TextWrapWord
			right.Add(container.NewBorder(nil, nil, nil, del, lbl))
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
		for _, r := range touching {
			r := r
			del := widget.NewButton("✕", func() {
				fail(store.DeleteRelations(ctx, []knowledge.Relation{r}))
				reload()
			})
			lbl := widget.NewLabel(fmt.Sprintf("%s  —%s→  %s", r.From, r.RelationType, r.To))
			lbl.Wrapping = fyne.TextWrapWord
			right.Add(container.NewBorder(nil, nil, nil, del, lbl))
		}
		// Add relation: this entity --relType--> target (another entity).
		relType := widget.NewEntry()
		relType.SetPlaceHolder("relation (e.g. owns)")
		targets := otherEntityNames(graph, e.Name)
		target := widget.NewSelect(targets, nil)
		if len(targets) > 0 {
			target.SetSelected(targets[0])
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
		name := widget.NewEntry()
		name.SetPlaceHolder("Name, e.g. Tyr Mactire")
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
