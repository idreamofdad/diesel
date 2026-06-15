package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// transcriptPlaceholder fills the empty transcript, mirroring the Qt build's
// placeholder text. It's cleared the moment the first turn lands.
const transcriptPlaceholder = "(Conversation will appear here)"

// transcriptView is the read-only conversation log: a vertical stack of turn
// rows inside a scroll. Each row is a bold, speaker-colored "<who>:" name beside
// the message body, and the body is a *selectable* Label so the conversation
// text can be selected and copied — Fyne's RichText (which the log used before)
// cannot be selected. All methods must run on the Fyne UI goroutine.
type transcriptView struct {
	list   *fyne.Container
	scroll *container.Scroll
	empty  bool
}

func newTranscriptView() *transcriptView {
	list := container.NewVBox()
	t := &transcriptView{list: list, scroll: container.NewVScroll(list)}
	t.reset()
	return t
}

// object returns the scrollable widget to place in a layout.
func (t *transcriptView) object() fyne.CanvasObject { return t.scroll }

// reset returns the transcript to its empty, placeholder state.
func (t *transcriptView) reset() {
	ph := widget.NewLabel(transcriptPlaceholder)
	ph.Importance = widget.LowImportance
	t.list.Objects = []fyne.CanvasObject{ph}
	t.list.Refresh()
	t.empty = true
}

// clear drops all turns. Named to match the hub's EventCleared handling.
func (t *transcriptView) clear() { t.reset() }

// appendTurn adds one turn row — a bold "<who>:" name in the given theme color
// (Primary for Diesel, Error for You/errors) beside the selectable body — and
// scrolls it into view.
func (t *transcriptView) appendTurn(who, body string, label fyne.ThemeColorName) {
	if t.empty {
		t.list.Objects = nil
		t.empty = false
	}
	name := widget.NewLabelWithStyle(who+":", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	name.Importance = labelImportance(label)

	body2 := widget.NewLabel(body)
	body2.Wrapping = fyne.TextWrapWord
	body2.Selectable = true

	// VBox-wrap the name so it stays top-aligned with the body's first line when
	// the body wraps to several lines.
	t.list.Add(container.NewBorder(nil, nil, container.NewVBox(name), nil, body2))
	t.scroll.ScrollToBottom()
}

// labelImportance maps a speaker's theme color to a Label Importance, which is
// how widget.Label exposes text color (HighImportance → Primary, etc.).
func labelImportance(c fyne.ThemeColorName) widget.Importance {
	switch c {
	case theme.ColorNamePrimary:
		return widget.HighImportance
	case theme.ColorNameError:
		return widget.DangerImportance
	default:
		return widget.MediumImportance
	}
}
