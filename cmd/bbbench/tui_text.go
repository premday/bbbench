package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// scrollKey moves off for a scroll key; ok is false for any other key.
func scrollKey(key string, off, win int) (int, bool) {
	switch key {
	case "up":
		return off - 1, true
	case "down":
		return off + 1, true
	case "pgup":
		return off - max(1, win), true
	case "pgdown":
		return off + max(1, win), true
	}
	return off, false
}

// wrapParts wraps s to width, each line starting with indent. It breaks at
// spaces, starts a new line after each "; " (the parts of an error), keeps
// the line breaks of s and cuts a word longer than the line.
func wrapParts(s string, width int, indent string) []string {
	return wrapLines(s, width, indent, true)
}

// wrapProse is wrapParts for plain prose (a screen message, a list of
// reasons): a "; " does not start a new line.
func wrapProse(s string, width int, indent string) []string {
	return wrapLines(s, width, indent, false)
}

// wrapLines is the shared wrapper; with parts false a "; " does not start a
// new line.
func wrapLines(s string, width int, indent string, parts bool) []string {
	w := max(1, width-utf8.RuneCountInString(indent))
	var out []string
	for _, para := range strings.Split(s, "\n") {
		segs := []string{para}
		if parts {
			segs = strings.SplitAfter(para, "; ")
		}
		for _, seg := range segs {
			words := strings.Fields(seg)
			if len(words) == 0 {
				if seg == para {
					out = append(out, "")
				}
				continue
			}
			line := ""
			for _, word := range words {
				for utf8.RuneCountInString(word) > w {
					if line != "" {
						out = append(out, indent+line)
						line = ""
					}
					r := []rune(word)
					out = append(out, indent+string(r[:w]))
					word = string(r[w:])
				}
				switch {
				case line == "":
					line = word
				case utf8.RuneCountInString(line)+1+utf8.RuneCountInString(word) <= w:
					line += " " + word
				default:
					out = append(out, indent+line)
					line = word
				}
			}
			if line != "" {
				out = append(out, indent+line)
			}
		}
	}
	return out
}

// clip cuts s to width, marking the cut with "...".
func clip(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width <= 3 {
		return string(r[:max(0, width)])
	}
	return string(r[:width-3]) + "..."
}

// window is a scrolled part of a list: the indicator line, then the
// visible lines from off.
type window struct {
	lines    []string
	win, off int
}

// scrollWindow shows body in room lines (one of them the indicator "<what>
// a-b of n"), from off, clamped.
func scrollWindow(body []string, room, off int, what string, width int) window {
	win := max(1, room-1)
	if win >= len(body) {
		win, off = len(body), 0
	}
	off = max(0, min(off, len(body)-win))
	ind := fmt.Sprintf("  %s %d-%d of %d", what, off+1, off+win, len(body))
	if win < len(body) {
		ind += ", up/down to scroll"
	}
	lines := append([]string{clip(ind, width)}, body[off:off+win]...)
	return window{lines: lines, win: win, off: off}
}
