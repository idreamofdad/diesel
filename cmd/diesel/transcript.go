//go:build cgo

package main

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// transcriptPlaceholder fills the empty transcript, mirroring the Qt
// build's placeholder text. It's cleared the moment the first turn lands.
const transcriptPlaceholder = "(Conversation will appear here)"

// transcriptView is the read-only conversation log: a RichText inside a
// vertical scroll. It replaces the Qt QTextEdit + chat.AppendTurn pairing.
//
// Each turn is two segments — a bold, colored "<who>: " label (Inline, so
// it opens the row) followed by the body (non-Inline, which closes the row
// after merging). RichText handles newlines and word-wrap within the body
// itself, so multi-line replies keep their line breaks. All methods must
// run on the Fyne UI goroutine.
type transcriptView struct {
	rich   *widget.RichText
	scroll *container.Scroll
	empty  bool
}

func newTranscriptView() *transcriptView {
	rich := widget.NewRichText()
	rich.Wrapping = fyne.TextWrapWord
	t := &transcriptView{rich: rich, scroll: container.NewVScroll(rich)}
	t.reset()
	return t
}

// object returns the scrollable widget to place in a layout.
func (t *transcriptView) object() fyne.CanvasObject { return t.scroll }

// reset returns the transcript to its empty, placeholder state.
func (t *transcriptView) reset() {
	t.rich.Segments = []widget.RichTextSegment{
		&widget.TextSegment{
			Text:  transcriptPlaceholder,
			Style: widget.RichTextStyle{ColorName: theme.ColorNamePlaceHolder},
		},
	}
	t.empty = true
	t.rich.Refresh()
}

// clear drops all turns. Named to match the hub's EventCleared handling.
func (t *transcriptView) clear() { t.reset() }

// appendTurn adds one "<who>: <body>" paragraph, the label rendered in the
// given theme color (Primary for Diesel, Error for You/errors), and scrolls
// the newest turn into view.
func (t *transcriptView) appendTurn(who, body string, label fyne.ThemeColorName) {
	if t.empty {
		t.rich.Segments = nil
		t.empty = false
	}
	t.rich.Segments = append(t.rich.Segments,
		&widget.TextSegment{
			Text:  who + ": ",
			Style: widget.RichTextStyle{ColorName: label, TextStyle: fyne.TextStyle{Bold: true}, Inline: true},
		},
		&widget.TextSegment{
			Text:  body,
			Style: widget.RichTextStyle{ColorName: theme.ColorNameForeground},
		},
	)
	t.rich.Refresh()
	t.scroll.ScrollToBottom()
}
