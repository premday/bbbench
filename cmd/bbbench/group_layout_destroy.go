package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

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

var (
	destroyHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	destroyGrayStyle   = lipgloss.NewStyle().Faint(true)
)

// destroyState is the picker and the selected entry. gen tells the answers
// of an earlier opening of the picker apart: they are dropped.
type destroyState struct {
	gen     int
	loading bool
	entries []settings.Existing
	notes   []string
	checked []bool
	reasons [][]string // why entry i may not be destroyed, first reason first
	cursor  int
	sel     settings.Existing
	err     string // the failed Destroy, verbatim
}

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
	entry settings.Existing
	err   error
	rep   *inuse.Report
	rerr  error
}

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
		if msg.gen != d.gen || msg.idx >= len(d.entries) {
			return m, nil
		}
		d.checked, d.reasons = slices.Clone(d.checked), slices.Clone(d.reasons)
		d.checked[msg.idx], d.reasons[msg.idx] = true, msg.reasons
		m.destroy = d
	case destroyedMsg:
		if m.step != layoutStepDestroyRun {
			return m, nil
		}
		// The discovered drives are the same disks after a teardown (it
		// wipes them, it does not add or remove any), so only what the
		// in-use scan says about them changes.
		m.applyInUse(msg.rep, msg.rerr)
		if msg.err != nil {
			m.destroy.err = msg.err.Error()
			m.step = layoutStepDestroyFailed
			return m, nil
		}
		m.step = layoutStepList
		m.msg = destroyedMessage(msg.entry, msg.rep, msg.rerr)
	}
	return m, nil
}

// destroyedMessage says which of e's disks are free now; a disk the re-scan
// still reports busy is named with the reason instead.
func destroyedMessage(e settings.Existing, rep *inuse.Report, rerr error) string {
	if rerr != nil || rep == nil {
		why := "no report"
		if rerr != nil {
			why = rerr.Error()
		}
		return fmt.Sprintf("%s destroyed; the in-use check failed (%s), so no disk is offered", e.Name, why)
	}
	var free, busy []string
	for _, d := range e.Disks {
		if why := rep.Reason(d.Name); why != "" {
			busy = append(busy, fmt.Sprintf("%s is still in use: %s", d.Name, why))
		} else {
			free = append(free, d.Name)
		}
	}
	parts := []string{e.Name + " destroyed"}
	if len(free) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s free", strings.Join(free, ", "), plural(len(free), "is", "are")))
	}
	return strings.Join(append(parts, busy...), "; ")
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
			d.sel = d.entries[d.cursor]
			m.step = layoutStepDestroyConfirm
		}
	}
	return m, nil
}

func (m layoutModel) updateDestroyConfirm(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "enter":
		// Enter does nothing here: only y destroys.
		return m, nil
	case "y":
		m.step = layoutStepDestroyRun
		s, e, rescan := m.setting, m.destroy.sel, m.rescan
		return m, func() tea.Msg {
			err := s.Destroy(context.Background(), e)
			logDestroy(e, err)
			rep, rerr := rescan()
			return destroyedMsg{entry: e, err: err, rep: rep, rerr: rerr}
		}
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

func logDestroy(e settings.Existing, err error) {
	if logger == nil {
		return
	}
	if err != nil {
		logger.Warn("destroying an existing configuration failed", "name", e.Name, "backend", e.Backend, "disks", diskNames(e.Disks), "error", err)
		return
	}
	logger.Info("destroyed an existing configuration", "name", e.Name, "backend", e.Backend, "disks", diskNames(e.Disks))
}

func (m layoutModel) destroyPickView(b *strings.Builder) {
	d := m.destroy
	b.WriteString("\n  Destroy an existing RAID configuration (up/down, Enter to select, Esc to go back)\n\n")
	switch {
	case d.loading:
		b.WriteString("  looking for existing configurations...\n")
	case len(d.entries) == 0:
		b.WriteString("  no existing RAID configuration found\n")
	}
	for i, e := range d.entries {
		row := fmt.Sprintf("%s  %s  %s", e.Name, e.Summary, strings.Join(diskNames(e.Disks), ", "))
		c := cursorMark(i == d.cursor)
		switch {
		case !d.checked[i]:
			fmt.Fprintf(b, "  %s- %s\n", c, destroyGrayStyle.Render(row+"  (checking...)"))
		case len(d.reasons[i]) > 0:
			fmt.Fprintf(b, "  %s- %s\n", c, destroyGrayStyle.Render(row+"  ("+d.reasons[i][0]+")"))
			if i == d.cursor {
				for _, r := range d.reasons[i] {
					fmt.Fprintf(b, "        not possible: %s\n", r)
				}
			}
		default:
			fmt.Fprintf(b, "  %s  %s\n", c, row)
		}
	}
	if len(d.notes) > 0 {
		b.WriteString("\n  Notes:\n")
		for _, n := range d.notes {
			b.WriteString("    " + n + "\n")
		}
	}
}

func (m layoutModel) destroyConfirmView(b *strings.Builder) {
	e := m.destroy.sel
	b.WriteString("\n  " + destroyHeaderStyle.Render("THIS DESTROYS DATA") + "\n\n")
	fmt.Fprintf(b, "  %s: %s\n", e.Name, e.Summary)
	if len(e.Stack) > 0 {
		b.WriteString("\n  Destroyed with it:\n")
		for _, s := range e.Stack {
			b.WriteString("    " + s + "\n")
		}
	}
	b.WriteString("\n  Disks:\n")
	for _, t := range e.Disks {
		fmt.Fprintf(b, "    %s  %s (S/N %s)\n", t.Name, strings.TrimSpace(t.Vendor+" "+t.Model), t.Serial)
	}
	b.WriteString("\n  The data on these disks is not kept. Disks that are mounted, used as swap or holding bbbench or its output are refused.\n")
	b.WriteString("\n  Press y to destroy, any other key to cancel\n")
}

func (m layoutModel) destroyRunView(b *strings.Builder) {
	fmt.Fprintf(b, "\n  destroying %s ... do not interrupt\n", m.destroy.sel.Name)
}

func (m layoutModel) destroyFailedView(b *strings.Builder) {
	fmt.Fprintf(b, "\n  Destroying %s failed:\n\n", m.destroy.sel.Name)
	for _, l := range strings.Split(m.destroy.err, "\n") {
		b.WriteString("    " + l + "\n")
	}
	b.WriteString("\n  Press any key to return to the list.\n")
}
