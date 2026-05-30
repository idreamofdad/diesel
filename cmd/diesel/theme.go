//go:build cgo

package main

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// dieselTheme reproduces the dark palette the Qt build hand-rolled in its
// stylesheet: near-black grays throughout, a soft blue as the primary
// accent, and a muted red for errors. It wraps the default theme so fonts,
// icons, sizes, and any color we don't override come from Fyne, and it
// always answers as the dark variant regardless of the OS appearance — the
// app was only ever designed dark.
//
// Primary and Error matter beyond chrome: the transcript renders speaker
// labels via RichText, which can only color text by theme color name, so
// "Diesel" rides ColorNamePrimary (blue) and "You"/"Error" ride
// ColorNameError (red) — see transcript.go.
type dieselTheme struct{ fyne.Theme }

func newDieselTheme() fyne.Theme { return &dieselTheme{Theme: theme.DefaultTheme()} }

// Palette lifted from the Qt stylesheet in the pre-Fyne main.go.
var (
	colBackground  = color.NRGBA{R: 0x2b, G: 0x2b, B: 0x2b, A: 0xff} // window/widget bg
	colForeground  = color.NRGBA{R: 0xec, G: 0xec, B: 0xec, A: 0xff} // body text
	colInputBg     = color.NRGBA{R: 0x23, G: 0x23, B: 0x23, A: 0xff} // text/input/list panes
	colButton      = color.NRGBA{R: 0x4a, G: 0x4a, B: 0x4a, A: 0xff} // buttons
	colPrimary     = color.NRGBA{R: 0x5a, G: 0xa7, B: 0xff, A: 0xff} // accent + Diesel label
	colError       = color.NRGBA{R: 0xe5, G: 0x73, B: 0x73, A: 0xff} // error + You label
	colBorder      = color.NRGBA{R: 0x3a, G: 0x3a, B: 0x3a, A: 0xff} // borders/dividers
	colPlaceholder = color.NRGBA{R: 0x88, G: 0x88, B: 0x88, A: 0xff} // hint text
	colSelection   = color.NRGBA{R: 0x55, G: 0x55, B: 0x55, A: 0xff} // text selection
)

func (t *dieselTheme) Color(name fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	switch name {
	case theme.ColorNameBackground:
		return colBackground
	case theme.ColorNameForeground:
		return colForeground
	case theme.ColorNameInputBackground:
		return colInputBg
	case theme.ColorNameMenuBackground, theme.ColorNameOverlayBackground:
		return colInputBg
	case theme.ColorNameButton, theme.ColorNameDisabledButton:
		return colButton
	case theme.ColorNamePrimary:
		return colPrimary
	case theme.ColorNameError:
		return colError
	case theme.ColorNameInputBorder, theme.ColorNameSeparator:
		return colBorder
	case theme.ColorNamePlaceHolder:
		return colPlaceholder
	case theme.ColorNameSelection:
		return colSelection
	}
	// Force the dark variant for everything we don't special-case so the
	// app looks the same under a light OS appearance.
	return t.Theme.Color(name, theme.VariantDark)
}
