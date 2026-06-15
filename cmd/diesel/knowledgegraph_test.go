package main

import (
	"image"
	"image/color"
	"testing"

	"diesel/internal/knowledge"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
)

// TestGphStep_ConvergesInBounds checks the ported spring simulation settles
// (kinetic energy decays toward zero) and keeps every node inside the world box.
func TestGphStep_ConvergesInBounds(t *testing.T) {
	const world = 600.0
	nodes := []gNode{
		{name: "a", x: 100, y: 100, r: 16},
		{name: "b", x: 120, y: 110, r: 16},
		{name: "c", x: 500, y: 400, r: 16},
		{name: "d", x: 300, y: 300, r: 16},
	}
	links := []gLink{{s: 0, t: 1, rel: "x"}, {s: 1, t: 2, rel: "y"}}

	first := gphStep(nodes, links, world, world)
	var energy float64
	for i := 0; i < 600; i++ {
		energy = gphStep(nodes, links, world, world)
	}
	if energy >= first || energy > 0.5 {
		t.Fatalf("expected the sim to settle: first=%v final=%v", first, energy)
	}
	for _, n := range nodes {
		if n.x < n.r-0.5 || n.x > world-n.r+0.5 || n.y < n.r-0.5 || n.y > world-n.r+0.5 {
			t.Fatalf("node %q escaped the world box: (%v, %v)", n.name, n.x, n.y)
		}
	}
}

// TestGphStep_FixedNodeStays checks a pinned (dragged) node holds its position
// while its neighbour is free to move.
func TestGphStep_FixedNodeStays(t *testing.T) {
	const world = 600.0
	nodes := []gNode{
		{name: "pinned", x: 200, y: 200, r: 16, fixed: true},
		{name: "free", x: 210, y: 205, r: 16},
	}
	links := []gLink{{s: 0, t: 1, rel: "x"}}
	for i := 0; i < 50; i++ {
		gphStep(nodes, links, world, world)
	}
	if nodes[0].x != 200 || nodes[0].y != 200 {
		t.Fatalf("pinned node moved to (%v, %v)", nodes[0].x, nodes[0].y)
	}
	if nodes[1].x == 210 && nodes[1].y == 205 {
		t.Fatal("free node never moved")
	}
}

// TestGraphWidget_SelectAndSearch exercises the widget end-to-end: it builds and
// lays out, selecting a node fires onSelect, and search selects+marks the match.
func TestGraphWidget_SelectAndSearch(t *testing.T) {
	var selected string
	gw := newGraphWidget(func(name string) { selected = name })
	win := test.NewWindow(gw)
	defer win.Close()
	win.Resize(fyne.NewSize(800, 600))

	gw.setGraph(knowledge.Graph{
		Entities: []knowledge.Entity{
			{Name: "Ada", Type: "person", Observations: []string{"likes math"}},
			{Name: "Mochi", Type: "cat"},
		},
		Relations: []knowledge.Relation{{From: "Ada", To: "Mochi", RelationType: "owns"}},
	})
	gw.stopSim() // don't leave the animation running past the test

	if len(gw.nodes) != 2 || len(gw.links) != 1 {
		t.Fatalf("want 2 nodes / 1 link, got %d / %d", len(gw.nodes), len(gw.links))
	}

	gw.setSelected(0)
	if selected != gw.nodes[0].name {
		t.Fatalf("onSelect = %q, want %q", selected, gw.nodes[0].name)
	}

	gw.focusOn("mochi")
	if gw.match < 0 || gw.nodes[gw.match].name != "Mochi" {
		t.Fatalf("focusOn did not match Mochi (match=%d)", gw.match)
	}
	if selected != "Mochi" {
		t.Fatalf("focusOn should select Mochi, selected=%q", selected)
	}
}

// TestGraphWidget_RendersPixels guards the "blank graph" failure mode: once the
// widget has a real size and a populated graph, the raster must actually paint
// something (the bug was the raster never redrawing after the size arrived).
func TestGraphWidget_RendersPixels(t *testing.T) {
	test.NewApp() // theme.Color needs a current app

	gw := newGraphWidget(nil)
	rend := gw.CreateRenderer()
	gw.setGraph(knowledge.Graph{
		Entities:  []knowledge.Entity{{Name: "A", Type: "person"}, {Name: "B", Type: "cat"}},
		Relations: []knowledge.Relation{{From: "A", To: "B", RelationType: "likes"}},
	})
	gw.stopSim()
	rend.Layout(fyne.NewSize(400, 300)) // sets size + transform, like a real layout

	img, ok := gw.draw(400, 300).(*image.RGBA)
	if !ok {
		t.Fatal("draw did not return *image.RGBA")
	}
	painted := false
	for i := 3; i < len(img.Pix); i += 4 { // alpha bytes
		if img.Pix[i] != 0 {
			painted = true
			break
		}
	}
	if !painted {
		t.Fatal("graph rendered an empty image — no bubbles or edges were drawn")
	}
}

// TestDrawLabel_Paints verifies the bundled font loads and node labels actually
// rasterize (the entity-name labels drawn under each bubble).
func TestDrawLabel_Paints(t *testing.T) {
	face := labelFace(20)
	if face == nil {
		t.Fatal("labelFace returned nil — font failed to load")
	}
	img := image.NewRGBA(image.Rect(0, 0, 160, 48))
	drawLabel(img, face, "Beckett", 80, 6, color.NRGBA{R: 255, G: 255, B: 255, A: 255})
	painted := 0
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0 {
			painted++
		}
	}
	if painted == 0 {
		t.Fatal("drawLabel painted nothing")
	}
}
