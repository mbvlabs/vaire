package ui

import (
	"strings"

	"rsc.io/qr"
)

// qrQuiet is the light border around the code, in modules.
const qrQuiet = 2

// QR renders text as a QR code for a terminal: two modules per character
// cell with half blocks, black on bright white whatever the terminal's own
// colours, so a phone camera reads it on light and dark themes alike.
func QR(text string) (string, error) {
	code, err := qr.Encode(text, qr.M)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	lo, hi := -qrQuiet, code.Size+qrQuiet
	for y := lo; y < hi; y += 2 {
		b.WriteString("\x1b[30;107m")
		for x := lo; x < hi; x++ {
			top, bottom := code.Black(x, y), y+1 < hi && code.Black(x, y+1)
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		b.WriteString("\x1b[0m\n")
	}
	return b.String(), nil
}
