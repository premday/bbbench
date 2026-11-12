package main

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"bbbench/internal/inuse"
	"bbbench/internal/settings"
)

// The x action of the layout list: pick one existing configuration, confirm
// with a single y, destroy it through the setting's safety gate and re-run
// the in-use scan. The screens know the providers only through
// settings.LayoutSetting and never name one. Nothing is recorded in the
// recovery file: nothing is built, so there is nothing to restore.
//
// Every destroy screen fits the terminal (bubbletea keeps the bottom of an
// over-tall view): long lines are wrapped or cut to the width, and the
// lists that can grow (entries, disks, an error) scroll in a window.

var (
	destroyHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	destroyGrayStyle   = lipgloss.NewStyle().Faint(true)
)

// The terminal size until the first tea.WindowSizeMsg.
const defaultWidth, defaultHeight = 80, 24

// destroyState is the picker and the selected entry. gen tells the answers
// of an earlier opening of the picker (or an earlier run) apart: they are
// dropped.
type destroyState struct {
	gen     int
	loading bool
	entries []settings.Existing
	notes   []string
	checked []bool
	reasons [][]string // why entry i may not be destroyed, first reason first
	cursor  int
	sel     settings.Existing
	scroll  int    // first line of the confirmation's disks or of the error
	err     string // the failed Destroy, verbatim

	started  time.Time
	elapsed  time.Duration
	cancel   context.CancelFunc
	stopping bool
}

type destroyTickMsg struct{ gen int }

type existingMsg struct {
	gen     int
	entries []settings.Existing
	errs    []error
}

type blockersMsg struct {
	gen     int
	idx     int
	reasons []string
}

type destroyedMsg struct {
	gen   int
	entry settings.Existing
	err   error
	rep   *inuse.Report
	rerr  error
}

func defaultTick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return destroyTickMsg{} })
}

func (m layoutModel) dims() (int, int) {
	w, h := m.width, m.height
	if w <= 0 {
		w = defaultWidth
	}
	if h <= 0 {
		h = defaultHeight
	}
	return w, h
}

// destroyers counts the backends that can list existing configurations.
func (m layoutModel) destroyers() int {
	n := 0
	for _, b := range m.setting.Backends() {
		if _, ok := b.(settings.Destroyer); ok {
			n++
		}
	}
	return n
}

// canDestroy says whether x is offered.
func (m layoutModel) canDestroy() bool { return m.rescan != nil && m.destroyers() > 0 }

// selectable says entry i was checked and nothing blocks it.
func (d destroyState) selectable(i int) bool {
	return i >= 0 && i < len(d.entries) && d.checked[i] && len(d.reasons[i]) == 0
}

func (m layoutModel) openDestroyPick() (tea.Model, tea.Cmd) {
	m.destroy = destroyState{gen: m.destroy.gen + 1, loading: true}
	m.step = layoutStepDestroyPick
	s, gen := m.setting, m.destroy.gen
	return m, func() tea.Msg {
		es, errs := s.Existing(context.Background())
		return existingMsg{gen: gen, entries: es, errs: errs}
	}
}

// updateDestroyMsg takes the answers of the commands the x action started.
func (m layoutModel) updateDestroyMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case existingMsg:
		if msg.gen != m.destroy.gen || m.step != layoutStepDestroyPick {
			return m, nil
		}
		d := destroyState{gen: msg.gen, entries: msg.entries}
		for _, err := range msg.errs {
			d.notes = append(d.notes, err.Error())
		}
		d.checked = make([]bool, len(msg.entries))
		d.reasons = make([][]string, len(msg.entries))
		var cmds []tea.Cmd
		s := m.setting
		for i, e := range msg.entries {
			if e.Blocked != "" {
				d.checked[i], d.reasons[i] = true, []string{e.Blocked}
				continue
			}
			cmds = append(cmds, func() tea.Msg {
				bl, err := s.DestroyBlockers(context.Background(), e)
				if err != nil {
					bl = append([]string{"check failed: " + err.Error()}, bl...)
				}
				return blockersMsg{gen: msg.gen, idx: i, reasons: bl}
			})
		}
		m.destroy = d
		return m, tea.Batch(cmds...)
	case blockersMsg:
		d := m.destroy
		if msg.gen != d.gen || m.step != layoutStepDestroyPick || msg.idx >= len(d.entries) {
			return m, nil
		}
		d.checked, d.reasons = slices.Clone(d.checked), slices.Clone(d.reasons)
		d.checked[msg.idx], d.reasons[msg.idx] = true, msg.reasons
		m.destroy = d
	case destroyTickMsg:
		if m.step != layoutStepDestroyRun {
			return m, nil
		}
		m.destroy.elapsed = time.Since(m.destroy.started)
		return m, m.tick()
	case destroyedMsg:
		if msg.gen != m.destroy.gen || m.step != layoutStepDestroyRun {
			return m, nil
		}
		if m.destroy.cancel != nil {
			m.destroy.cancel()
		}
		// The discovered drives are the same disks after a teardown (it
		// wipes them, it does not add or remove any), so only what the
		// in-use scan says about them changes.
		m.applyInUse(msg.rep, msg.rerr)
		if msg.err != nil {
			m.destroy.err, m.destroy.scroll = msg.err.Error(), 0
			m.step = layoutStepDestroyFailed
			m.destroyOutcomes = append(slices.Clip(m.destroyOutcomes), "destroying "+msg.entry.Name+" failed: "+msg.err.Error())
			return m, nil
		}
		m.step = layoutStepList
		m.msg = m.destroyedMessage(msg.entry)
		m.destroyOutcomes = append(slices.Clip(m.destroyOutcomes), m.msg)
	}
	return m, nil
}

// destroyedMessage says which of e's disks can be used now (offered after
// the re-scan); the others are named with the reason.
func (m layoutModel) destroyedMessage(e settings.Existing) string {
	known := drivesByName(m.drives)
	var free, other []string
	for _, d := range e.Disks {
		_, found := known[d.Name]
		why, gray := m.grayed[d.Name]
		switch {
		case !found:
			other = append(other, d.Name+" is not among the discovered disks")
		case gray && m.busy[d.Name]:
			other = append(other, fmt.Sprintf("%s is still in use: %s", d.Name, why))
		case gray:
			other = append(other, fmt.Sprintf("%s is not offered: %s", d.Name, why))
		default:
			free = append(free, d.Name)
		}
	}
	parts := []string{e.Name + " destroyed"}
	if len(free) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s free", strings.Join(free, ", "), plural(len(free), "is", "are")))
	}
	return strings.Join(append(parts, other...), "; ")
}

func (m layoutModel) updateDestroyPick(key string) (tea.Model, tea.Cmd) {
	d := &m.destroy
	switch key {
	case "q":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		d.gen++ // drop the answers still coming
		m.step = layoutStepList
	case "up", "k":
		if d.cursor > 0 {
			d.cursor--
		}
	case "down", "j":
		if d.cursor < len(d.entries)-1 {
			d.cursor++
		}
	case "enter":
		if !d.loading && d.selectable(d.cursor) {
			d.sel, d.scroll = d.entries[d.cursor], 0
			m.step = layoutStepDestroyConfirm
		}
	}
	return m, nil
}

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

func (m layoutModel) updateDestroyConfirm(key string) (tea.Model, tea.Cmd) {
	if off, ok := scrollKey(key, m.destroy.scroll, m.confirmLayout().win); ok {
		m.destroy.scroll = off
		m.destroy.scroll = m.confirmLayout().off
		return m, nil
	}
	switch key {
	case "enter":
		// Enter does nothing here: only y destroys.
		return m, nil
	case "y":
		return m.startDestroy()
	}
	m.step = layoutStepList
	return m, nil
}

// startDestroy runs Destroy and then the re-scan as one command. Ctrl+C
// cancels its ctx: the provider finishes the step in flight and starts no
// other.
func (m layoutModel) startDestroy() (tea.Model, tea.Cmd) {
	ctx, cancel := context.WithCancel(context.Background())
	m.destroy.gen++
	m.destroy.cancel, m.destroy.stopping = cancel, false
	m.destroy.started, m.destroy.elapsed = time.Now(), 0
	m.step = layoutStepDestroyRun
	s, e, rescan, gen := m.setting, m.destroy.sel, m.rescan, m.destroy.gen
	run := func() tea.Msg {
		err := s.Destroy(ctx, e)
		logDestroy(e, err)
		rep, rerr := rescan()
		return destroyedMsg{gen: gen, entry: e, err: err, rep: rep, rerr: rerr}
	}
	return m, tea.Batch(run, m.tick())
}

// updateDestroyRun takes the keys while Destroy runs: only the first Ctrl+C
// does something (it asks the provider to stop between steps).
func (m layoutModel) updateDestroyRun(key string) (tea.Model, tea.Cmd) {
	if key == "ctrl+c" && !m.destroy.stopping {
		m.destroy.stopping = true
		if m.destroy.cancel != nil {
			m.destroy.cancel()
		}
	}
	return m, nil
}

func (m layoutModel) updateDestroyFailed(key string) (tea.Model, tea.Cmd) {
	if off, ok := scrollKey(key, m.destroy.scroll, m.failedLayout().win); ok {
		m.destroy.scroll = off
		m.destroy.scroll = m.failedLayout().off
		return m, nil
	}
	m.step = layoutStepList
	return m, nil
}

func diskNames(ts []settings.Target) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// printDestroyOutcomes writes what each destroy of the layout screens did,
// so that it stays on the terminal once the TUI is gone.
func printDestroyOutcomes(w io.Writer, outcomes []string) {
	if len(outcomes) == 0 {
		return
	}
	fmt.Fprintln(w, "Existing configurations destroyed from the layout screen:")
	for _, o := range outcomes {
		fmt.Fprintln(w, "  - "+o)
	}
}

func logDestroy(e settings.Existing, err error) {
	if logger == nil {
		return
	}
	if err != nil {
		logger.Warn("destroying an existing configuration failed", "name", e.Name, "backend", e.Backend, "disks", diskNames(e.Disks), "error", err.Error())
		return
	}
	logger.Info("destroyed an existing configuration", "name", e.Name, "backend", e.Backend, "disks", diskNames(e.Disks))
}

// wrapText wraps s to width, each line starting with indent. It breaks at
// spaces, starts a new line after each "; " (the parts of an error), keeps
// the line breaks of s and cuts a word longer than the line.
func wrapText(s string, width int, indent string) []string {
	return wrapTextOpt(s, width, indent, true)
}

// wrapTextOpt is wrapText; with parts false a "; " does not start a new line
// (plain prose: a screen message, a list of reasons).
func wrapTextOpt(s string, width int, indent string, parts bool) []string {
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

// confirmLayout is the confirmation screen fitted to the terminal.
func (m layoutModel) confirmLayout() window {
	w, h := m.dims()
	e := m.destroy.sel
	head := []string{"  " + destroyHeaderStyle.Render(clip("THIS DESTROYS DATA", w-2))}
	head = append(head, wrapText(e.Name+": "+e.Summary, w, "  ")...)
	var stack []string
	for _, s := range e.Stack {
		stack = append(stack, clip("    "+s, w))
	}
	prompt := wrapText("Press y to destroy, any other key to cancel", w, "  ")
	sentence := wrapText("The data on these disks is not kept. Destroy checks again that nothing of it is in use and refuses if something is.", w, "  ")
	stackHead := 0
	if len(stack) > 0 {
		stackHead = 1
	}
	// The disks need their indicator and at least one line.
	room := h - len(head) - stackHead - len(stack) - len(prompt)
	if room < 2 && len(stack) > 0 {
		keep := max(0, len(stack)-(2-room)-1)
		more := len(stack) - keep
		stack = append(stack[:keep], clip(fmt.Sprintf("    ... %d more; enlarge the terminal to see them", more), w))
		room = h - len(head) - stackHead - len(stack) - len(prompt)
	}
	foot := prompt
	if room-1-len(sentence) >= min(3, len(e.Disks)) {
		foot = append(slices.Clone(sentence), prompt...)
		room -= len(sentence)
	}
	var body []string
	for _, t := range e.Disks {
		body = append(body, clip(fmt.Sprintf("    %s  %s (S/N %s)", t.Name, strings.TrimSpace(t.Vendor+" "+t.Model), t.Serial), w))
	}
	disks := scrollWindow(body, room, m.destroy.scroll, "disks", w)
	lines := head
	if len(stack) > 0 {
		lines = append(lines, clip("  Destroyed with it:", w))
		lines = append(lines, stack...)
	}
	lines = append(lines, disks.lines...)
	disks.lines = append(lines, foot...)
	return disks
}

// failedLayout is the failure screen: the error wrapped to the width in a
// scrolled window, the way back on the last line.
func (m layoutModel) failedLayout() window {
	w, h := m.dims()
	head := wrapText("Destroying "+m.destroy.sel.Name+" failed:", w, "  ")
	foot := wrapText("up/down to scroll, any other key returns to the list", w, "  ")
	body := wrapText(m.destroy.err, w, "    ")
	win := scrollWindow(body, h-len(head)-len(foot), m.destroy.scroll, "lines", w)
	win.lines = append(append(head, win.lines...), foot...)
	return win
}

func (m layoutModel) destroyPickLines() []string {
	w, h := m.dims()
	d := m.destroy
	head := wrapText("Destroy an existing RAID configuration (up/down, Enter to select, Esc to go back)", w, "  ")
	switch {
	case d.loading:
		head = append(head, "  looking for existing configurations...")
	case len(d.entries) == 0 && len(d.notes) >= m.destroyers():
		head = append(head, wrapText("no provider could list existing configurations", w, "  ")...)
	case len(d.entries) == 0:
		head = append(head, wrapText("no existing RAID configuration found", w, "  ")...)
	}
	// Each entry is one line; the highlighted one is followed by all its
	// reasons. The window keeps it in view.
	var body []string
	from, to := 0, 0
	for i, e := range d.entries {
		row := fmt.Sprintf("%s  %s  %s", e.Name, e.Summary, strings.Join(diskNames(e.Disks), ", "))
		c := cursorMark(i == d.cursor)
		if i == d.cursor {
			from = len(body)
		}
		switch {
		case !d.checked[i]:
			body = append(body, "  "+c+"- "+destroyGrayStyle.Render(clip(row+"  (checking...)", w-6)))
		case len(d.reasons[i]) > 0:
			body = append(body, "  "+c+"- "+destroyGrayStyle.Render(clip(row+"  ("+d.reasons[i][0]+")", w-6)))
			if i == d.cursor {
				for _, r := range d.reasons[i] {
					body = append(body, wrapText("not possible: "+r, w, "        ")...)
				}
			}
		default:
			body = append(body, clip("  "+c+"  "+row, w))
		}
		if i == d.cursor {
			to = len(body)
		}
	}
	var foot []string
	if len(d.notes) > 0 {
		foot = append(foot, "  Notes:")
		for _, n := range d.notes {
			foot = append(foot, wrapText(n, w, "    ")...)
		}
		if limit := max(2, h/3); len(foot) > limit {
			more := len(foot) - (limit - 1)
			foot = append(foot[:limit-1], clip(fmt.Sprintf("    ... %d more note lines", more), w))
		}
	}
	if m.msg != "" {
		foot = append(foot, wrapText(m.msg, w, "  ")...)
	}
	room := h - len(head) - len(foot)
	if len(body) <= room {
		return append(append(head, body...), foot...)
	}
	win := max(1, room-1)
	off := max(0, min(from, to-win))
	end := min(len(body), off+win)
	ind := clip(fmt.Sprintf("  entry %d of %d, up/down to move", d.cursor+1, len(d.entries)), w)
	return append(append(append(head, ind), body[off:end]...), foot...)
}

func (m layoutModel) destroyRunLines() []string {
	w, _ := m.dims()
	lines := wrapText(fmt.Sprintf("destroying %s ... %d s, do not interrupt", m.destroy.sel.Name, int(m.destroy.elapsed.Seconds())), w, "  ")
	if m.destroy.stopping {
		lines = append(lines, wrapText("stopping after the step in flight finishes; nothing new is started", w, "  ")...)
	}
	return lines
}

// destroyView is the screen of a destroy step, fitted to the terminal.
func (m layoutModel) destroyView() string {
	var lines []string
	switch m.step {
	case layoutStepDestroyPick:
		lines = m.destroyPickLines()
	case layoutStepDestroyConfirm:
		lines = m.confirmLayout().lines
	case layoutStepDestroyRun:
		lines = m.destroyRunLines()
	case layoutStepDestroyFailed:
		lines = m.failedLayout().lines
	}
	return strings.Join(lines, "\n")
}
