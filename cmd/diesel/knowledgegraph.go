package main

import (
	"image"
	"image/color"
	"math"
	"strings"
	"time"

	"diesel/internal/knowledge"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"golang.org/x/image/vector"
)

// graphWidget renders the knowledge graph as a force-directed node-link diagram
// — the desktop twin of the web's KnowledgeGraph.svelte. Entities are bubbles
// (colored by type, sized by observation count), relations are lines. The
// layout is the same dependency-free spring simulation re-expressed in Go, run
// on a Fyne animation that halts once the graph settles to spare the CPU. The
// circles and edges are drawn into one canvas.Raster (via x/image/vector) so a
// few-hundred-node graph stays cheap; only the selected/hovered/searched node
// gets a canvas.Text label. Bubbles drag, the background pans, the wheel zooms,
// and a search centers on a match.
//
// It edits nothing itself: tapping a bubble calls onSelect(name) so the hosting
// dialog points its shared edit panel at that entity.

// Spring-simulation tuning, ported from the web graph (tuned there for a
// few-dozen-node graph; the larger logical world below keeps hundreds legible).
const (
	gphRepulsion = 7000.0
	gphLinkLen   = 130.0
	gphSpring    = 0.02
	gphCenter    = 0.006
	gphDamping   = 0.85
	gphSettle    = 0.02 // per-node kinetic-energy floor to stop animating
	gphMaxTicks  = 1500 // hard stop (~25s at 60fps) if it never quite settles
)

type gNode struct {
	name       string
	typ        string
	x, y       float64
	vx, vy     float64
	r          float64
	fixed      bool
}

type gLink struct {
	s, t int
	rel  string
}

// gphStep advances the spring simulation one tick over a w×h world and returns
// the total kinetic energy left; the caller stops animating once that falls
// below a threshold. Pure and deterministic given the input — unit-tested.
func gphStep(nodes []gNode, links []gLink, w, h float64) float64 {
	n := nodes
	// Pairwise repulsion (O(n²) — fine for the few-hundred-node target).
	for i := 0; i < len(n); i++ {
		for j := i + 1; j < len(n); j++ {
			dx := n[j].x - n[i].x
			dy := n[j].y - n[i].y
			d2 := dx*dx + dy*dy
			if d2 < 0.01 {
				d2 = 0.01
			}
			d := math.Sqrt(d2)
			f := gphRepulsion / d2
			fx := dx / d * f
			fy := dy / d * f
			n[i].vx -= fx
			n[i].vy -= fy
			n[j].vx += fx
			n[j].vy += fy
		}
	}
	// Link springs.
	for _, l := range links {
		a, b := &n[l.s], &n[l.t]
		dx := b.x - a.x
		dy := b.y - a.y
		d := math.Sqrt(dx*dx + dy*dy)
		if d < 0.01 {
			d = 0.01
		}
		f := (d - gphLinkLen) * gphSpring
		fx := dx / d * f
		fy := dy / d * f
		a.vx += fx
		a.vy += fy
		b.vx -= fx
		b.vy -= fy
	}
	// Centering gravity + integrate, clamped to the world box.
	energy := 0.0
	for i := range n {
		nd := &n[i]
		nd.vx += (w/2 - nd.x) * gphCenter
		nd.vy += (h/2 - nd.y) * gphCenter
		nd.vx *= gphDamping
		nd.vy *= gphDamping
		if !nd.fixed {
			nd.x += nd.vx
			nd.y += nd.vy
		}
		nd.x = clampF(nd.x, nd.r, w-nd.r)
		nd.y = clampF(nd.y, nd.r, h-nd.r)
		energy += nd.vx*nd.vx + nd.vy*nd.vy
	}
	return energy
}

type graphWidget struct {
	widget.BaseWidget

	nodes []gNode
	links []gLink
	world float64 // logical world is a square of this side

	// View transform: screenDP = world*scale + off.
	scale      float64
	offX, offY float64

	selected int // node index, -1 = none
	selName  string
	hovered  int
	match    int // search hit, -1 = none

	dragging  bool
	dragNode  int // node being dragged, -1 = panning the background
	lastMouse fyne.Position
	size      fyne.Size

	fitted bool
	ticks  int
	anim   *fyne.Animation

	onSelect func(string)
	renderer *graphRenderer
}

var (
	_ fyne.Widget       = (*graphWidget)(nil)
	_ fyne.Tappable     = (*graphWidget)(nil)
	_ fyne.Draggable    = (*graphWidget)(nil)
	_ fyne.Scrollable   = (*graphWidget)(nil)
	_ desktop.Hoverable = (*graphWidget)(nil)
)

func newGraphWidget(onSelect func(string)) *graphWidget {
	g := &graphWidget{selected: -1, hovered: -1, match: -1, dragNode: -1, onSelect: onSelect}
	g.ExtendBaseWidget(g)
	return g
}

// setGraph (re)loads the graph, reusing existing bubbles by name so their
// settled positions survive an edit — only genuinely new entities are seeded,
// and the selection is re-resolved by name so it survives a rename.
func (g *graphWidget) setGraph(kg knowledge.Graph) {
	prev := make(map[string]gNode, len(g.nodes))
	for _, n := range g.nodes {
		prev[n.name] = n
	}
	g.world = math.Max(600, 90*math.Sqrt(float64(len(kg.Entities))))
	idx := make(map[string]int, len(kg.Entities))
	nodes := make([]gNode, 0, len(kg.Entities))
	for i, e := range kg.Entities {
		idx[e.Name] = i
		r := 16 + math.Min(float64(len(e.Observations)), 6)*2
		if p, ok := prev[e.Name]; ok {
			p.typ = e.Type
			p.r = r
			p.fixed = false
			nodes = append(nodes, p)
			continue
		}
		a := float64(i) / math.Max(1, float64(len(kg.Entities))) * 2 * math.Pi
		nodes = append(nodes, gNode{
			name: e.Name, typ: e.Type, r: r,
			x: g.world/2 + math.Cos(a)*g.world*0.25,
			y: g.world/2 + math.Sin(a)*g.world*0.25,
		})
	}
	g.nodes = nodes
	links := make([]gLink, 0, len(kg.Relations))
	for _, r := range kg.Relations {
		s, ok1 := idx[r.From]
		t, ok2 := idx[r.To]
		if ok1 && ok2 {
			links = append(links, gLink{s: s, t: t, rel: r.RelationType})
		}
	}
	g.links = links

	g.selected = -1
	if g.selName != "" {
		if i, ok := idx[g.selName]; ok {
			g.selected = i
		} else {
			g.selName = ""
		}
	}
	g.match = -1
	g.hovered = -1
	// Pre-relax the layout synchronously so the graph renders already settled —
	// the animation may not tick until a frame or two after the view toggle, and
	// we don't want to flash an empty or exploded graph in the meantime.
	for i := 0; i < 400; i++ {
		if gphStep(g.nodes, g.links, g.world, g.world) < gphSettle*float64(len(g.nodes)+1) {
			break
		}
	}
	g.fitted = false
	g.ensureInitialView()
	g.startSim()
	g.refresh()
}

func (g *graphWidget) CreateRenderer() fyne.WidgetRenderer {
	raster := canvas.NewRaster(g.draw)
	labels := make([]*canvas.Text, 3)
	for i := range labels {
		t := canvas.NewText("", theme.Color(theme.ColorNameForeground))
		t.TextSize = theme.CaptionTextSize()
		labels[i] = t
	}
	g.renderer = &graphRenderer{g: g, raster: raster, labels: labels}
	return g.renderer
}

// ── View transform & hit testing (all in widget DP space) ──

func (g *graphWidget) ensureInitialView() {
	if g.scale > 0 || g.size.Width <= 0 || g.world <= 0 {
		return
	}
	s := math.Min(float64(g.size.Width), float64(g.size.Height)) / g.world * 0.9
	g.scale = s
	g.offX = float64(g.size.Width)/2 - g.world/2*s
	g.offY = float64(g.size.Height)/2 - g.world/2*s
}

func (g *graphWidget) hit(p fyne.Position) int {
	best, bestD := -1, math.MaxFloat64
	for i := range g.nodes {
		sx := g.nodes[i].x*g.scale + g.offX
		sy := g.nodes[i].y*g.scale + g.offY
		rr := g.nodes[i].r * g.scale
		dx := float64(p.X) - sx
		dy := float64(p.Y) - sy
		if d := dx*dx + dy*dy; d <= rr*rr && d < bestD {
			bestD, best = d, i
		}
	}
	return best
}

func (g *graphWidget) autofit() {
	if len(g.nodes) == 0 || g.size.Width <= 0 {
		return
	}
	minX, minY := math.MaxFloat64, math.MaxFloat64
	maxX, maxY := -math.MaxFloat64, -math.MaxFloat64
	for _, n := range g.nodes {
		minX, minY = math.Min(minX, n.x-n.r), math.Min(minY, n.y-n.r)
		maxX, maxY = math.Max(maxX, n.x+n.r), math.Max(maxY, n.y+n.r)
	}
	cw, ch := math.Max(1, maxX-minX), math.Max(1, maxY-minY)
	vw, vh := float64(g.size.Width), float64(g.size.Height)
	const margin = 28.0
	s := clampF(math.Min((vw-2*margin)/cw, (vh-2*margin)/ch), 0.05, 3)
	g.scale = s
	g.offX = vw/2 - (minX+maxX)/2*s
	g.offY = vh/2 - (minY+maxY)/2*s
}

// focusOn selects and centers the first entity matching query (exact name,
// else substring; case-insensitive) and marks it as the search hit.
func (g *graphWidget) focusOn(query string) {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		g.match = -1
		g.refresh()
		return
	}
	best := -1
	for i := range g.nodes {
		name := strings.ToLower(g.nodes[i].name)
		if name == q {
			best = i
			break
		}
		if best < 0 && strings.Contains(name, q) {
			best = i
		}
	}
	g.match = best
	if best < 0 {
		g.refresh()
		return
	}
	if g.scale < 0.6 {
		g.scale = 0.6
	}
	g.offX = float64(g.size.Width)/2 - g.nodes[best].x*g.scale
	g.offY = float64(g.size.Height)/2 - g.nodes[best].y*g.scale
	g.setSelected(best)
}

func (g *graphWidget) setSelected(i int) {
	g.selected = i
	name := ""
	if i >= 0 && i < len(g.nodes) {
		name = g.nodes[i].name
	}
	g.selName = name
	if g.onSelect != nil {
		g.onSelect(name)
	}
	g.refresh()
}

// ── Pointer interaction ──

func (g *graphWidget) Tapped(e *fyne.PointEvent) { g.setSelected(g.hit(e.Position)) }

func (g *graphWidget) Dragged(e *fyne.DragEvent) {
	if !g.dragging {
		g.dragging = true
		g.dragNode = g.hit(g.lastMouse) // press point: grab a bubble, else pan
		if g.dragNode >= 0 {
			g.nodes[g.dragNode].fixed = true
		}
	}
	if g.dragNode >= 0 && g.scale > 0 {
		n := &g.nodes[g.dragNode]
		n.x += float64(e.Dragged.DX) / g.scale
		n.y += float64(e.Dragged.DY) / g.scale
		n.vx, n.vy = 0, 0
		g.startSim()
		return
	}
	g.offX += float64(e.Dragged.DX)
	g.offY += float64(e.Dragged.DY)
	g.refresh()
}

func (g *graphWidget) DragEnd() {
	if g.dragNode >= 0 && g.dragNode < len(g.nodes) {
		g.nodes[g.dragNode].fixed = false
	}
	g.dragging = false
	g.dragNode = -1
	g.startSim()
}

func (g *graphWidget) Scrolled(e *fyne.ScrollEvent) {
	if g.scale <= 0 {
		return
	}
	ns := clampF(g.scale*math.Pow(1.0015, float64(e.Scrolled.DY)), 0.02, 8)
	cx, cy := float64(e.Position.X), float64(e.Position.Y)
	g.offX = cx - (cx-g.offX)*(ns/g.scale)
	g.offY = cy - (cy-g.offY)*(ns/g.scale)
	g.scale = ns
	g.refresh()
}

func (g *graphWidget) MouseIn(e *desktop.MouseEvent) { g.lastMouse = e.Position }
func (g *graphWidget) MouseMoved(e *desktop.MouseEvent) {
	g.lastMouse = e.Position
	if i := g.hit(e.Position); i != g.hovered {
		g.hovered = i
		g.refresh()
	}
}
func (g *graphWidget) MouseOut() {
	if g.hovered != -1 {
		g.hovered = -1
		g.refresh()
	}
}

// ── Simulation loop ──

func (g *graphWidget) startSim() {
	if g.anim != nil {
		return
	}
	g.ticks = 0
	a := fyne.NewAnimation(time.Second, func(float32) {
		g.ticks++
		e := gphStep(g.nodes, g.links, g.world, g.world)
		settled := (!g.dragging && e < gphSettle*float64(len(g.nodes)+1)) || g.ticks > gphMaxTicks
		if settled && !g.fitted && g.size.Width > 0 {
			g.autofit()
			g.fitted = true
		}
		g.refresh()
		if settled {
			g.stopSim()
		}
	})
	a.RepeatCount = fyne.AnimationRepeatForever
	a.Curve = fyne.AnimationLinear
	g.anim = a
	a.Start()
}

func (g *graphWidget) stopSim() {
	if g.anim != nil {
		g.anim.Stop()
		g.anim = nil
	}
}

func (g *graphWidget) refresh() {
	if g.renderer != nil {
		canvas.Refresh(g)
	}
}

// ── Rendering ──

// draw paints all edges and bubbles into the raster image. w,h are device
// pixels; events arrive in DP, so positions are scaled by pixels-per-DP.
func (g *graphWidget) draw(w, h int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if len(g.nodes) == 0 || g.size.Width <= 0 || g.scale <= 0 {
		return img
	}
	ppd := float64(w) / float64(g.size.Width)
	tx := func(wx, wy float64) (float32, float32) {
		return float32((wx*g.scale + g.offX) * ppd), float32((wy*g.scale + g.offY) * ppd)
	}
	ras := vector.NewRasterizer(w, h)

	edgeCol := theme.Color(theme.ColorNameSeparator)
	lw := float32(1.5 * ppd)
	for _, l := range g.links {
		a, b := g.nodes[l.s], g.nodes[l.t]
		x1, y1 := tx(a.x, a.y)
		x2, y2 := tx(b.x, b.y)
		fillLine(ras, img, x1, y1, x2, y2, lw, edgeCol)
	}

	ringCol := theme.Color(theme.ColorNameForeground)
	matchCol := theme.Color(theme.ColorNamePrimary)
	for i := range g.nodes {
		n := g.nodes[i]
		cx, cy := tx(n.x, n.y)
		rad := float32(n.r * g.scale * ppd)
		if i == g.selected || i == g.match {
			rc := ringCol
			if i == g.match {
				rc = matchCol
			}
			fillCircle(ras, img, cx, cy, rad+float32(2.5*ppd), rc)
		}
		fillCircle(ras, img, cx, cy, rad, colorForType(n.typ))
	}
	return img
}

// fillCircle rasterizes an antialiased filled circle, compositing only within
// the circle's bounding box so hundreds of bubbles stay cheap. The path is
// built in box-local coordinates because vector.Rasterizer maps its (0,0) to
// the draw rectangle's Min — drawing in full-image coords with a sub-rect would
// read an empty mask and paint nothing.
func fillCircle(ras *vector.Rasterizer, dst *image.RGBA, cx, cy, r float32, c color.Color) {
	if r <= 0 {
		return
	}
	bb := image.Rect(int(math.Floor(float64(cx-r))), int(math.Floor(float64(cy-r))),
		int(math.Ceil(float64(cx+r))), int(math.Ceil(float64(cy+r)))).Intersect(dst.Bounds())
	if bb.Empty() {
		return
	}
	cx, cy = cx-float32(bb.Min.X), cy-float32(bb.Min.Y)
	const k = 0.5522847498307936
	ras.Reset(bb.Dx(), bb.Dy())
	ras.MoveTo(cx+r, cy)
	ras.CubeTo(cx+r, cy+r*k, cx+r*k, cy+r, cx, cy+r)
	ras.CubeTo(cx-r*k, cy+r, cx-r, cy+r*k, cx-r, cy)
	ras.CubeTo(cx-r, cy-r*k, cx-r*k, cy-r, cx, cy-r)
	ras.CubeTo(cx+r*k, cy-r, cx+r, cy-r*k, cx+r, cy)
	ras.ClosePath()
	ras.Draw(dst, bb, image.NewUniform(c), image.Point{})
}

// fillLine rasterizes an antialiased thick line as a filled quad, in box-local
// coordinates (see fillCircle for why).
func fillLine(ras *vector.Rasterizer, dst *image.RGBA, x1, y1, x2, y2, width float32, c color.Color) {
	dx, dy := float64(x2-x1), float64(y2-y1)
	ln := math.Hypot(dx, dy)
	if ln < 1e-6 {
		return
	}
	nx := float32(-dy/ln) * width / 2
	ny := float32(dx/ln) * width / 2
	bb := image.Rect(
		int(math.Floor(math.Min(float64(x1), float64(x2))-float64(width))),
		int(math.Floor(math.Min(float64(y1), float64(y2))-float64(width))),
		int(math.Ceil(math.Max(float64(x1), float64(x2))+float64(width))),
		int(math.Ceil(math.Max(float64(y1), float64(y2))+float64(width))),
	).Intersect(dst.Bounds())
	if bb.Empty() {
		return
	}
	ox, oy := float32(bb.Min.X), float32(bb.Min.Y)
	x1, y1, x2, y2 = x1-ox, y1-oy, x2-ox, y2-oy
	ras.Reset(bb.Dx(), bb.Dy())
	ras.MoveTo(x1+nx, y1+ny)
	ras.LineTo(x2+nx, y2+ny)
	ras.LineTo(x2-nx, y2-ny)
	ras.LineTo(x1-nx, y1-ny)
	ras.ClosePath()
	ras.Draw(dst, bb, image.NewUniform(c), image.Point{})
}

// colorForType hashes the entity type to a stable hue — the same scheme the web
// graph uses so a "person" or "cat" reads the same color across both UIs.
func colorForType(typ string) color.Color {
	if typ == "" {
		typ = "thing"
	}
	h := 0
	for _, c := range typ {
		h = (h*31 + int(c)) % 360
	}
	if h < 0 {
		h += 360
	}
	return hslColor(float64(h), 0.55, 0.55)
}

func hslColor(h, s, l float64) color.Color {
	c := (1 - math.Abs(2*l-1)) * s
	hp := h / 60
	x := c * (1 - math.Abs(math.Mod(hp, 2)-1))
	var r, g, b float64
	switch {
	case hp < 1:
		r, g, b = c, x, 0
	case hp < 2:
		r, g, b = x, c, 0
	case hp < 3:
		r, g, b = 0, c, x
	case hp < 4:
		r, g, b = 0, x, c
	case hp < 5:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	m := l - c/2
	return color.NRGBA{R: uint8((r + m) * 255), G: uint8((g + m) * 255), B: uint8((b + m) * 255), A: 255}
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// graphRenderer draws the raster full-bleed and floats the (few) labels over it.
type graphRenderer struct {
	g      *graphWidget
	raster *canvas.Raster
	labels []*canvas.Text
}

func (r *graphRenderer) Destroy() { r.g.stopSim() }

func (r *graphRenderer) Layout(size fyne.Size) {
	g := r.g
	g.size = size
	r.raster.Resize(size)
	r.raster.Move(fyne.NewPos(0, 0))
	g.ensureInitialView()
	// First real size after a (re)load — frame the graph now that the viewport
	// is known. Guarded by fitted so user pan/zoom and plain window resizes
	// don't snap the view back.
	if !g.fitted && size.Width > 0 && len(g.nodes) > 0 {
		g.autofit()
		g.fitted = true
	}
	r.updateLabels()
	// Force the raster to regenerate with the now-valid size/transform. Without
	// this the view keeps showing the pre-size (empty) image once the sim has
	// settled — the cause of the blank graph after the first toggle.
	canvas.Refresh(r.raster)
}

func (r *graphRenderer) MinSize() fyne.Size { return fyne.NewSize(360, 360) }

func (r *graphRenderer) Objects() []fyne.CanvasObject {
	objs := make([]fyne.CanvasObject, 0, 1+len(r.labels))
	objs = append(objs, r.raster)
	for _, t := range r.labels {
		objs = append(objs, t)
	}
	return objs
}

func (r *graphRenderer) Refresh() {
	r.updateLabels()
	canvas.Refresh(r.raster)
}

// updateLabels names only the selected, hovered, and searched bubbles, reusing
// a small fixed pool of text objects (blanked when unused).
func (r *graphRenderer) updateLabels() {
	g := r.g
	want := make([]int, 0, len(r.labels))
	add := func(i int) {
		if i < 0 || i >= len(g.nodes) || len(want) >= len(r.labels) {
			return
		}
		for _, w := range want {
			if w == i {
				return
			}
		}
		want = append(want, i)
	}
	add(g.selected)
	add(g.hovered)
	add(g.match)

	for k, t := range r.labels {
		if k >= len(want) {
			t.Text = ""
			t.Hide()
			canvas.Refresh(t)
			continue
		}
		n := g.nodes[want[k]]
		t.Text = n.name
		if want[k] == g.match {
			t.Color = theme.Color(theme.ColorNamePrimary)
		} else {
			t.Color = theme.Color(theme.ColorNameForeground)
		}
		sz := fyne.MeasureText(n.name, t.TextSize, t.TextStyle)
		sx := n.x*g.scale + g.offX
		sy := n.y*g.scale + g.offY
		t.Move(fyne.NewPos(float32(sx)-sz.Width/2, float32(sy+n.r*g.scale)+2))
		t.Show()
		canvas.Refresh(t)
	}
}
