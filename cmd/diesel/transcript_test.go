package main

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/widget"
)

// TestTranscript_BodyIsSelectable guards that appended message bodies are
// rendered as selectable Labels (so the conversation can be copied).
func TestTranscript_BodyIsSelectable(t *testing.T) {
	test.NewApp()

	tv := newTranscriptView()
	tv.appendTurn("Diesel", "hello world", labelDiesel)

	found := false
	for _, row := range tv.list.Objects {
		c, ok := row.(*fyne.Container)
		if !ok {
			continue
		}
		for _, o := range c.Objects {
			if lbl, ok := o.(*widget.Label); ok && lbl.Selectable && lbl.Text == "hello world" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("expected a selectable body Label containing the message text")
	}
}
