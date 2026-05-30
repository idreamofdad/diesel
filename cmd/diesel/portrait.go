package main

import (
	"bytes"
	"image"
	_ "image/jpeg" // ComfyUI preview frames arrive as JPEG
	_ "image/png"  // final portraits arrive as PNG

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// portraitWidth is the fixed on-screen width of the portrait column, matched
// to the Qt build. Height follows the image's aspect ratio.
const portraitWidth = 300

// tappableBox wraps an arbitrary canvas object so it can react to taps. Fyne
// images aren't interactive on their own, so this is how the portrait gets
// its "double-click to view full size" behavior and the full-size viewer
// gets "tap to dismiss".
type tappableBox struct {
	widget.BaseWidget
	content  fyne.CanvasObject
	onTap    func()
	onDouble func()
}

func newTappableBox(content fyne.CanvasObject) *tappableBox {
	b := &tappableBox{content: content}
	b.ExtendBaseWidget(b)
	return b
}

func (b *tappableBox) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(b.content)
}

func (b *tappableBox) Tapped(_ *fyne.PointEvent) {
	if b.onTap != nil {
		b.onTap()
	}
}

func (b *tappableBox) DoubleTapped(_ *fyne.PointEvent) {
	if b.onDouble != nil {
		b.onDouble()
	}
}

// portraitPane is the right-hand column: a "Diesel" header over the most
// recent character portrait. It mirrors the Qt portrait widget — a fixed
// width, a placeholder until the first image lands, and a double-click that
// opens the last fully-rendered PNG at full size.
type portraitPane struct {
	root        *fyne.Container
	image       *canvas.Image
	placeholder *widget.Label
	stack       *fyne.Container
	latestPNG   []byte // last final (non-preview) image, for the viewer
}

func newPortraitPane(win fyne.Window) *portraitPane {
	p := &portraitPane{}

	header := widget.NewLabelWithStyle("Diesel", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})

	p.image = canvas.NewImageFromImage(nil)
	p.image.FillMode = canvas.ImageFillContain
	p.image.SetMinSize(fyne.NewSize(portraitWidth, portraitWidth))
	p.image.Hide()

	p.placeholder = widget.NewLabel("(no portrait yet)")
	p.placeholder.Alignment = fyne.TextAlignCenter

	p.stack = container.NewStack(p.placeholder, p.image)
	box := newTappableBox(p.stack)
	box.onDouble = func() {
		if len(p.latestPNG) > 0 {
			showPortraitFullSize(win, p.latestPNG)
		}
	}

	p.root = container.NewBorder(header, nil, nil, nil, box)
	return p
}

func (p *portraitPane) object() fyne.CanvasObject { return p.root }

// show paints data into the portrait. final=false is for intermediate
// ComfyUI preview frames, which are displayed but not remembered as the
// image the full-size viewer opens — that always shows the last finished
// PNG, never a half-baked preview.
func (p *portraitPane) show(data []byte, final bool) {
	if len(data) == 0 {
		return
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return
	}
	if final {
		p.latestPNG = data
	}
	p.image.Image = img
	p.image.Show()
	p.placeholder.Hide()
	p.image.Refresh()
}

// showPortraitFullSize pops a borderless-feeling window with the portrait
// scaled up; a tap anywhere dismisses it. Height is capped so a tall image
// still fits a typical screen, with width following the aspect ratio.
func showPortraitFullSize(_ fyne.Window, png []byte) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(png))
	w, h := float32(600), float32(800)
	if err == nil && cfg.Width > 0 && cfg.Height > 0 {
		const maxH = 900
		if cfg.Height <= maxH {
			w, h = float32(cfg.Width), float32(cfg.Height)
		} else {
			w = float32(cfg.Width * maxH / cfg.Height)
			h = maxH
		}
	}

	img := canvas.NewImageFromReader(bytes.NewReader(png), "portrait.png")
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSize(w, h))

	win := fyne.CurrentApp().NewWindow("Diesel")
	box := newTappableBox(img)
	box.onTap = func() { win.Close() }
	win.SetContent(box)
	win.Resize(fyne.NewSize(w, h))
	win.Show()
}
