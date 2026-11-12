package main

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bbbench/internal/inuse"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// The layout setup screens: the list of layouts, the backend picker, the
// editor of one layout and the one destructive confirmation. They know the
// backends only through settings.Backend and settings.LayoutSetting, and
// build nothing: no command runs here except the x action
// (group_layout_destroy.go), which destroys an existing configuration only
// after its own confirmation.

type layoutStep int

const (
	layoutStepList layoutStep = iota
	layoutStepBackend
	layoutStepEditor
	layoutStepConfirm
	layoutStepDestroyPick
	layoutStepDestroyConfirm
	layoutStepDestroyRun
	layoutStepDestroyFailed
)

// layoutDraft is a layout being edited. backend is nil for the baseline.
type layoutDraft struct {
	backend settings.Backend
	level   string
	options map[string]string
	// chosen are the options the user set; the others follow the default
	// as the data disks change.
	chosen map[string]bool
	roles  map[string]settings.Role // disk name -> role; absent: unused
}

func (d layoutDraft) clone() layoutDraft {
	d.options = maps.Clone(d.options)
	d.chosen = maps.Clone(d.chosen)
	d.roles = maps.Clone(d.roles)
	return d
}

func (d layoutDraft) backendName() string {
	if d.backend == nil {
		return ""
	}
	return d.backend.Name()
}

// layoutEntry is a saved, valid layout.
type layoutEntry struct {
	draft layoutDraft
	value settings.Value
	disks []settings.Disk
}

type rowKind int

const (
	rowLevel rowKind = iota
	rowOption
	rowDisk
)

type editorRow struct {
	kind rowKind
	opt  settings.Option
	disk DriveInfo
}

type layoutModel struct {
	setting settings.LayoutSetting
	drives  []DriveInfo
	targets map[string]settings.Target
	// grayed maps a disk that cannot be selected to the reason; busy says
	// the reason is that it is in use (else the setting does not apply).
	grayed   map[string]string
	busy     map[string]bool
	all      string // non-empty: no disk can be used, and why
	warnings []string
	// carries maps a selectable disk to the data it still carries
	// (inuse.Report.Carries): shown, never blocking.
	carries map[string]string

	layouts []layoutEntry
	step    layoutStep
	cursor  int // list
	bcursor int // backend picker
	ecursor int // editor
	edit    layoutDraft
	editIdx int // -1: a new layout
	msg     string

	// rescan re-runs the in-use scan after a destroy; nil disables x.
	rescan  func() (*inuse.Report, error)
	destroy destroyState
	// destroyOutcomes is what each destroy did, in order: printed after
	// the TUI, which the alternate screen would otherwise take with it.
	destroyOutcomes []string
	// tick schedules the next update of the destroy run's elapsed time.
	tick          func() tea.Cmd
	width, height int // the terminal; 0 until the first size message

	done      bool
	cancelled bool
	result    *groupChoice
}

// newLayoutModel sets up the layout screens over the discovered drives. A
// disk is grayed when the in-use check failed, when it is in use, when the
// setting does not apply to it, or when it has no serial (a teardown finds
// the members by serial).
func newLayoutModel(s settings.LayoutSetting, drives []DriveInfo, targets map[string]settings.Target, busy *inuse.Report, busyErr error) layoutModel {
	m := layoutModel{setting: s, drives: drives, targets: targets, editIdx: -1, tick: defaultTick}
	m.applyInUse(busy, busyErr)
	return m
}

// applyInUse recomputes which disks are grayed, busy or carry data from an
// in-use report (or its error). The layouts already in the list are kept
// as they are; one whose disk is now busy is marked on the list and c
// refuses to continue until it is edited or deleted (busyLayout).
func (m *layoutModel) applyInUse(busy *inuse.Report, busyErr error) {
	s := m.setting
	m.grayed, m.busy = map[string]string{}, map[string]bool{}
	m.all, m.warnings, m.carries = "", nil, nil
	switch {
	case busyErr != nil:
		m.all = "in-use check failed: " + busyErr.Error()
	case busy == nil:
		m.all = "in-use check failed: no report"
	default:
		m.all = busy.All
		m.warnings = busy.Warnings
		m.carries = busy.Carries
	}
	for _, d := range m.drives {
		name := d.Device.Name
		if m.all != "" {
			m.grayed[name], m.busy[name] = m.all, true
			continue
		}
		if why := busy.Reason(name); why != "" {
			m.grayed[name], m.busy[name] = why, true
			continue
		}
		if ok, why := s.Applicable(m.target(d)); !ok {
			m.grayed[name] = why
			continue
		}
		if d.Device.Serial == "" {
			m.grayed[name] = "no serial: it could not be found again for a teardown"
		}
	}
}

// target is the setting target of d. Its identity and node come from the
// discovered drive itself, the same discovery the group's drives come from.
func (m layoutModel) target(d DriveInfo) settings.Target {
	t := m.targets[d.Device.Name]
	t.Name, t.Path = d.Device.Name, d.Device.Path
	t.Serial, t.Model, t.Vendor = d.Device.Serial, d.Device.Model, d.Device.Vendor
	t.Rotational = d.Device.Rotational
	return t
}

func (m layoutModel) Init() tea.Cmd { return nil }

// roles are the roles the draft's level takes, data first.
func (m layoutModel) roles(d layoutDraft) []settings.Role {
	if d.backend == nil {
		return []settings.Role{settings.RoleData}
	}
	return d.backend.Roles(d.level)
}

// disks are the draft's disks with their roles, in discovery order.
func (m layoutModel) disks(d layoutDraft) []settings.Disk {
	var out []settings.Disk
	for _, di := range m.drives {
		if r, ok := d.roles[di.Device.Name]; ok && r != "" {
			out = append(out, settings.Disk{Target: m.target(di), Role: r})
		}
	}
	return out
}

func dataCount(disks []settings.Disk) int {
	n := 0
	for _, d := range disks {
		if d.Role == settings.RoleData {
			n++
		}
	}
	return n
}

func (m layoutModel) layoutOf(d layoutDraft) settings.Layout {
	return settings.Layout{Backend: d.backendName(), Level: d.level, Options: maps.Clone(d.options)}
}

// reapply sets the options to what the backend lists for the level and the
// data disks: the options the user chose are kept while still possible, the
// others follow the default.
func (m layoutModel) reapply(d *layoutDraft) {
	if d.backend == nil {
		d.options = map[string]string{}
		return
	}
	in := map[string]string{}
	for k := range d.chosen {
		in[k] = d.options[k]
	}
	l := settings.WithDefaults(d.backend, settings.Layout{Backend: d.backendName(), Level: d.level, Options: in}, dataCount(m.disks(*d)))
	for k := range d.chosen {
		if l.Options[k] != in[k] {
			delete(d.chosen, k)
		}
	}
	d.options = l.Options
}

// check validates the draft and returns its value.
func (m layoutModel) check(d layoutDraft) (settings.Value, error) {
	return m.setting.LayoutValue(m.layoutOf(d), m.disks(d))
}

func (m layoutModel) rows() []editorRow {
	rows := []editorRow{{kind: rowLevel}}
	if m.edit.backend != nil {
		for _, o := range m.edit.backend.Options(m.edit.level, dataCount(m.disks(m.edit))) {
			rows = append(rows, editorRow{kind: rowOption, opt: o})
		}
	}
	for _, d := range m.drives {
		rows = append(rows, editorRow{kind: rowDisk, disk: d})
	}
	return rows
}

func (m layoutModel) freeDrives() []DriveInfo {
	var out []DriveInfo
	for _, d := range m.drives {
		if _, gray := m.grayed[d.Device.Name]; !gray {
			out = append(out, d)
		}
	}
	return out
}

func (m layoutModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width, m.height = ws.Width, ws.Height
		return m, nil
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m.updateDestroyMsg(msg)
	}
	key := k.String()
	if m.step == layoutStepDestroyRun {
		// A teardown is running: Ctrl+C only asks it to stop between steps.
		return m.updateDestroyRun(key)
	}
	if key == "ctrl+c" {
		m.cancelled = true
		return m, tea.Quit
	}
	m.msg = ""
	switch m.step {
	case layoutStepList:
		return m.updateList(key)
	case layoutStepBackend:
		return m.updateBackend(key)
	case layoutStepEditor:
		return m.updateEditor(key)
	case layoutStepConfirm:
		return m.updateConfirm(key)
	case layoutStepDestroyPick:
		return m.updateDestroyPick(key)
	case layoutStepDestroyConfirm:
		return m.updateDestroyConfirm(key)
	case layoutStepDestroyFailed:
		return m.updateDestroyFailed(key)
	}
	return m, nil
}

func (m layoutModel) updateList(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "q", "esc":
		m.cancelled = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.layouts)-1 {
			m.cursor++
		}
	case "a":
		m.addLayout()
	case "b":
		m.addBaseline()
	case "x":
		if m.canDestroy() {
			return m.openDestroyPick()
		}
	case "enter":
		if len(m.layouts) == 0 {
			return m, nil
		}
		m.editIdx = m.cursor
		m.edit = m.layouts[m.cursor].draft.clone()
		m.step, m.ecursor = layoutStepEditor, 0
	case "d":
		if len(m.layouts) == 0 {
			return m, nil
		}
		m.layouts = slices.Delete(m.layouts, m.cursor, m.cursor+1)
		m.cursor = max(0, min(m.cursor, len(m.layouts)-1))
	case "c":
		if len(m.layouts) < 2 {
			m.msg = "Add at least two layouts to compare (the no-array baseline counts as one)."
			return m, nil
		}
		for i, e := range m.layouts {
			if name, why := m.busyLayout(e); name != "" {
				m.msg = fmt.Sprintf("Layout %d (%s) uses %s, which is now in use: %s; edit or delete it.", i+1, e.value.Label, name, why)
				return m, nil
			}
		}
		m.step = layoutStepConfirm
	}
	return m, nil
}

// busyLayout is the first disk of e that is now in use (after a re-scan,
// or every disk when the re-scan failed) and why; "" when there is none.
func (m layoutModel) busyLayout(e layoutEntry) (string, string) {
	for _, d := range e.disks {
		if m.busy[d.Name] {
			return d.Name, m.grayed[d.Name]
		}
	}
	return "", ""
}

// addLayout opens the backend picker, or the editor at once when the setting
// has only one backend.
func (m *layoutModel) addLayout() {
	bs := m.setting.Backends()
	var avail []settings.Backend
	var why []string
	for _, b := range bs {
		if ok, r := b.Available(); ok {
			avail = append(avail, b)
		} else {
			why = append(why, b.Name()+": "+r)
		}
	}
	if len(avail) == 0 {
		if len(why) == 0 {
			why = []string{"none is registered"}
		}
		m.msg = "no layout provider is available: " + strings.Join(why, "; ")
		return
	}
	if len(bs) == 1 {
		m.openNew(avail[0])
		return
	}
	m.step = layoutStepBackend
	m.bcursor = slices.IndexFunc(bs, func(b settings.Backend) bool { ok, _ := b.Available(); return ok })
}

func (m *layoutModel) openNew(b settings.Backend) {
	d := layoutDraft{backend: b, level: b.Levels()[0], chosen: map[string]bool{}, roles: map[string]settings.Role{}}
	m.reapply(&d)
	m.edit, m.editIdx = d, -1
	m.step, m.ecursor = layoutStepEditor, 0
}

// addBaseline adds the baseline over every free disk.
func (m *layoutModel) addBaseline() {
	free := m.freeDrives()
	if len(free) == 0 {
		m.msg = "No free disk for the baseline."
		if m.all != "" {
			m.msg += " " + m.all
		}
		return
	}
	d := layoutDraft{level: settings.LevelNone, options: map[string]string{}, chosen: map[string]bool{}, roles: map[string]settings.Role{}}
	for _, di := range free {
		d.roles[di.Device.Name] = settings.RoleData
	}
	m.editIdx = -1
	m.save(d)
}

// save stores d as a layout (a new one when editIdx is -1) when it is valid
// and not already in the list; otherwise it sets the message.
func (m *layoutModel) save(d layoutDraft) bool {
	v, err := m.check(d)
	if err != nil {
		m.msg = "Not saved: " + err.Error()
		return false
	}
	for i, e := range m.layouts {
		if i != m.editIdx && e.value.ID == v.ID {
			m.msg = fmt.Sprintf("Not saved: layout %d is already %s; each layout must differ.", i+1, v.Label)
			return false
		}
	}
	e := layoutEntry{draft: d.clone(), value: v, disks: m.disks(d)}
	if m.editIdx >= 0 {
		m.layouts[m.editIdx] = e
		m.cursor = m.editIdx
	} else {
		m.layouts = append(m.layouts, e)
		m.cursor = len(m.layouts) - 1
	}
	m.step, m.editIdx = layoutStepList, -1
	return true
}

func (m layoutModel) updateBackend(key string) (tea.Model, tea.Cmd) {
	bs := m.setting.Backends()
	switch key {
	case "q":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		m.step = layoutStepList
	case "up", "k":
		if m.bcursor > 0 {
			m.bcursor--
		}
	case "down", "j":
		if m.bcursor < len(bs)-1 {
			m.bcursor++
		}
	case "enter":
		if m.bcursor < 0 || m.bcursor >= len(bs) {
			return m, nil
		}
		if ok, _ := bs[m.bcursor].Available(); ok {
			m.openNew(bs[m.bcursor])
		}
	}
	return m, nil
}

func cycle[T comparable](list []T, cur T, step int) T {
	if len(list) == 0 {
		return cur
	}
	i := slices.Index(list, cur)
	if i < 0 {
		return list[0]
	}
	return list[(i+step+len(list))%len(list)]
}

func isNoYes(o settings.Option) bool { return slices.Equal(o.Choices, []string{"no", "yes"}) }

func (m layoutModel) updateEditor(key string) (tea.Model, tea.Cmd) {
	rows := m.rows()
	m.ecursor = min(m.ecursor, len(rows)-1)
	row := rows[m.ecursor]
	switch key {
	case "esc":
		m.step, m.editIdx = layoutStepList, -1
	case "up", "k":
		if m.ecursor > 0 {
			m.ecursor--
		}
	case "down", "j":
		if m.ecursor < len(rows)-1 {
			m.ecursor++
		}
	case "left", "right", "h", "l":
		step := 1
		if key == "left" || key == "h" {
			step = -1
		}
		switch row.kind {
		case rowLevel:
			if m.edit.backend == nil {
				return m, nil
			}
			m.edit = m.edit.clone()
			m.edit.level = cycle(m.edit.backend.Levels(), m.edit.level, step)
			allowed := m.roles(m.edit)
			for n, r := range m.edit.roles {
				if !slices.Contains(allowed, r) {
					delete(m.edit.roles, n)
				}
			}
			m.reapply(&m.edit)
		case rowOption:
			m.setOption(row.opt, cycle(row.opt.Choices, m.edit.options[row.opt.Key], step))
		}
	case " ":
		switch row.kind {
		case rowOption:
			if isNoYes(row.opt) {
				m.setOption(row.opt, cycle(row.opt.Choices, m.edit.options[row.opt.Key], 1))
			}
		case rowDisk:
			name := row.disk.Device.Name
			if why, gray := m.grayed[name]; gray {
				if m.busy[name] {
					m.msg = fmt.Sprintf("%s is in use: %s", name, why)
				} else {
					m.msg = fmt.Sprintf("%s: %s", name, why)
				}
				return m, nil
			}
			m.edit = m.edit.clone()
			roles := m.roles(m.edit)
			cur, used := m.edit.roles[name]
			switch i := slices.Index(roles, cur); {
			case !used || cur == "":
				if len(roles) > 0 {
					m.edit.roles[name] = roles[0]
				}
			case i >= 0 && i+1 < len(roles):
				m.edit.roles[name] = roles[i+1]
			default:
				delete(m.edit.roles, name)
			}
			m.reapply(&m.edit)
		}
	case "enter":
		m.save(m.edit)
	}
	return m, nil
}

func (m *layoutModel) setOption(o settings.Option, v string) {
	m.edit = m.edit.clone()
	m.edit.options[o.Key] = v
	m.edit.chosen[o.Key] = true
	m.reapply(&m.edit)
}

func (m layoutModel) updateConfirm(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "q":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		m.step = layoutStepList
	case "y":
		m.result = m.choice()
		m.done = true
		return m, tea.Quit
	}
	// Enter does nothing here: only y starts.
	return m, nil
}

// choice is the confirmed group: one variant per layout in order, its
// values, the union of the disks in order of first use, sequential.
func (m layoutModel) choice() *groupChoice {
	c := &groupChoice{Setting: m.setting, Strategy: rungroup.Sequential, Excluded: maps.Clone(m.grayed)}
	byName := drivesByName(m.drives)
	seen := map[string]bool{}
	for _, e := range m.layouts {
		c.Variants = append(c.Variants, settings.Variant{Value: e.value, Disks: slices.Clone(e.disks)})
		c.Values = append(c.Values, e.value)
		for _, d := range e.disks {
			if !seen[d.Name] {
				seen[d.Name] = true
				c.Drives = append(c.Drives, byName[d.Name])
			}
		}
	}
	return c
}

// erased are the names of every disk of every layout, in discovery order.
func (m layoutModel) erased() []string {
	used := map[string]bool{}
	for _, e := range m.layouts {
		for _, d := range e.disks {
			used[d.Name] = true
		}
	}
	var out []string
	for _, d := range m.drives {
		if used[d.Device.Name] {
			out = append(out, d.Device.Name)
		}
	}
	return out
}

// outcome is the confirmed choice, or why there is none.
func (m layoutModel) outcome() (*groupChoice, error) {
	if m.cancelled || m.result == nil {
		return nil, errors.New("cancelled")
	}
	return m.result, nil
}

func (m layoutModel) View() string {
	if m.done || m.cancelled {
		return ""
	}
	var b strings.Builder
	switch m.step {
	case layoutStepList:
		m.listView(&b)
	case layoutStepBackend:
		m.backendView(&b)
	case layoutStepEditor:
		m.editorView(&b)
	case layoutStepConfirm:
		m.confirmView(&b)
	case layoutStepDestroyPick, layoutStepDestroyConfirm, layoutStepDestroyRun, layoutStepDestroyFailed:
		return m.destroyView()
	}
	if m.msg != "" {
		b.WriteString("\n")
		m.put(&b, m.msg, "  ", "  ")
	}
	return m.wrapView(b.String())
}

func cursorMark(on bool) string {
	if on {
		return "> "
	}
	return "  "
}

func (m layoutModel) listView(b *strings.Builder) {
	help := "a add, b add the no-array baseline, Enter edit, d delete, c continue, q quit"
	if m.canDestroy() {
		help += ", x destroy an existing RAID configuration"
	}
	b.WriteString("\n")
	m.put(b, "Layouts to compare ("+help+")", "  ", "    ")
	b.WriteString("\n")
	if m.all != "" {
		m.put(b, "No disk can be used: "+m.all, "  ", "    ")
		b.WriteString("\n")
	}
	if len(m.layouts) == 0 {
		b.WriteString("    (no layout yet)\n")
	}
	for i, e := range m.layouts {
		names := make([]string, 0, len(e.disks))
		for _, d := range e.disks {
			names = append(names, d.Name)
		}
		m.put(b, fmt.Sprintf("%d. %s: %s", i+1, e.value.Label, strings.Join(names, ", ")), "  "+cursorMark(i == m.cursor), "      ")
		if name, why := m.busyLayout(e); name != "" {
			m.put(b, fmt.Sprintf("disk %s is now in use: %s", name, why), "        ", "          ")
		}
	}
	if m.all == "" {
		var gray []string
		for _, d := range m.drives {
			if why, ok := m.grayed[d.Device.Name]; ok {
				gray = append(gray, d.Device.Name+": "+why)
			}
		}
		if len(gray) > 0 {
			b.WriteString("\n  Not offered:\n")
			for _, g := range gray {
				m.put(b, g, "    ", "        ")
			}
		}
	}
	for _, w := range m.warnings {
		b.WriteString("\n")
		m.put(b, "Warning: "+w, "  ", "    ")
	}
}

// put writes text wrapped to the terminal width: the first line starts with
// first, the others with rest (first and rest have the same display width,
// so a cursor mark in first does not shift the text). Without a size message
// the width is the default; nothing is cut at the right edge.
func (m layoutModel) put(b *strings.Builder, text, first, rest string) {
	w, _ := m.dims()
	lines := wrapProse(text, w, rest)
	if len(lines) == 0 {
		return
	}
	lines[0] = first + strings.TrimPrefix(lines[0], rest)
	b.WriteString(strings.Join(lines, "\n") + "\n")
}

func (m layoutModel) backendView(b *strings.Builder) {
	b.WriteString("\n  Which provider builds the layout? (up/down, Enter, Esc to go back)\n\n")
	for i, be := range m.setting.Backends() {
		line := be.Name()
		if ok, why := be.Available(); !ok {
			line += "  (not available: " + why + ")"
		}
		fmt.Fprintf(b, "  %s%s\n", cursorMark(i == m.bcursor), line)
	}
}

func roleBox(r settings.Role) string {
	if r == "" {
		return "[       ]"
	}
	return fmt.Sprintf("[%-7s]", r)
}

func (m layoutModel) editorView(b *strings.Builder) {
	n := m.editIdx + 1
	if m.editIdx < 0 {
		n = len(m.layouts) + 1
	}
	title := m.edit.backendName()
	if title == "" {
		title = "no-array baseline"
	}
	fmt.Fprintf(b, "\n  Layout %d (%s)  (up/down, left/right to change, Space for a disk's role, Enter to save, Esc to cancel)\n\n", n, title)
	for i, r := range m.rows() {
		c := cursorMark(i == m.ecursor)
		switch r.kind {
		case rowLevel:
			fmt.Fprintf(b, "  %slevel: %s\n", c, m.edit.level)
		case rowOption:
			fmt.Fprintf(b, "  %s%s: %s\n", c, r.opt.Label, m.edit.options[r.opt.Key])
		case rowDisk:
			d := r.disk.Device
			if why, gray := m.grayed[d.Name]; gray {
				fmt.Fprintf(b, "  %s  -       %s  %s %s  %s\n", c, d.Name, d.Vendor, d.Model, why)
				continue
			}
			fmt.Fprintf(b, "  %s%s %s  %s %s  %d GB%s\n", c, roleBox(m.edit.roles[d.Name]), d.Name, d.Vendor, d.Model, d.CapacityGB(), m.carriesNote(d.Name))
		}
	}
	if v, err := m.check(m.edit); err != nil {
		fmt.Fprintf(b, "\n  Not valid yet: %s\n", err)
	} else {
		fmt.Fprintf(b, "\n  Layout: %s\n", v.Label)
	}
	if w := m.capacityWarning(); w != "" {
		b.WriteString("  Warning: " + w + "\n")
	}
}

// capacityWarning says when the data members of a built layout differ in
// capacity.
func (m layoutModel) capacityWarning() string {
	if m.edit.backend == nil {
		return ""
	}
	byName := drivesByName(m.drives)
	var parts []string
	caps := map[uint64]bool{}
	for _, d := range m.disks(m.edit) {
		if d.Role != settings.RoleData {
			continue
		}
		gb := byName[d.Name].Device.CapacityGB()
		caps[gb] = true
		parts = append(parts, fmt.Sprintf("%s %d GB", d.Name, gb))
	}
	if len(caps) < 2 {
		return ""
	}
	return fmt.Sprintf("members differ in capacity (%s): the layout uses the smallest", strings.Join(parts, ", "))
}

// carriesNote is "  carries data: <what>" for a disk that still carries a
// filesystem or a partition table, else "".
func (m layoutModel) carriesNote(name string) string {
	if what := m.carries[name]; what != "" {
		return "  carries data: " + what
	}
	return ""
}

func (m layoutModel) confirmView(b *strings.Builder) {
	b.WriteString("\n  Confirm the layout test: THE DISKS BELOW WILL BE ERASED\n  (y to start, Esc to go back, q to quit)\n\n")
	byName := drivesByName(m.drives)
	var per time.Duration
	unknown, missing := 0, 0
	for i, e := range m.layouts {
		fmt.Fprintf(b, "  Run %d: %s\n", i+1, e.value.Label)
		for _, d := range e.disks {
			fmt.Fprintf(b, "      %s  %s %s (S/N %s)  %s%s\n", d.Name, d.Vendor, d.Model, d.Serial, d.Role, m.carriesNote(d.Name))
		}
		// A layout is one target: its first data disk stands for it. The
		// baseline benchmarks all its disks.
		var est []DriveInfo
		for _, d := range e.disks {
			if e.draft.backend == nil || d.Role == settings.RoleData {
				est = append(est, byName[d.Name])
				if e.draft.backend != nil {
					break
				}
			}
		}
		p, u, mi := estimateRun(est)
		per += p
		unknown = max(unknown, u)
		missing += mi
	}
	erased := m.erased()
	for i, name := range erased {
		if what := m.carries[name]; what != "" {
			erased[i] = name + " (carries data: " + what + ")"
		}
	}
	fmt.Fprintf(b, "\n  Disks erased: %s\n", strings.Join(erased, ", "))
	n := len(m.layouts)
	fmt.Fprintf(b, "\n  %d %s of the full test plan", n, plural(n, "run", "runs"))
	if missing > 0 && per == 0 {
		b.WriteString("; estimated time unknown until the fio files are generated")
	} else {
		fmt.Fprintf(b, "; estimated time %s", per.Round(time.Second))
		if unknown > 0 {
			fmt.Fprintf(b, " or more (%d %s of unknown length not counted)", unknown, plural(unknown, "phase", "phases"))
		}
	}
	b.WriteString(", plus the initial resync of each layout, which is not included\n")
	for _, w := range m.warnings {
		b.WriteString("\n  Warning: " + w + "\n")
	}
	b.WriteString("\n  Each layout is torn down and its members wiped after its run, also after an error or Ctrl+C.\n  Nothing is built before you press y.\n")
}

// runLayoutSetupTUI lets the user list the layouts to compare and confirm
// the erase once. Nothing is built here; the only change is the x action,
// which destroys one existing configuration the user selected and confirmed
// (through the setting's safety gate), then re-runs rescan. A nil rescan
// disables x.
func runLayoutSetupTUI(s settings.LayoutSetting, drives []DriveInfo, targets map[string]settings.Target, busy *inuse.Report, busyErr error, rescan func() (*inuse.Report, error)) (*groupChoice, error) {
	m := newLayoutModel(s, drives, targets, busy, busyErr)
	m.rescan = rescan
	return runLayoutModel(m, runTea, os.Stdout)
}

// runLayoutModel runs m with run, then writes the outcome of every destroy
// to out, after run has returned (and left the alternate screen).
func runLayoutModel(m layoutModel, run func(tea.Model) (tea.Model, error), out io.Writer) (*groupChoice, error) {
	final, err := run(m)
	if err != nil {
		return nil, err
	}
	lm := final.(layoutModel)
	printDestroyOutcomes(out, lm.destroyOutcomes)
	return lm.outcome()
}

// wrapView wraps every line wider than the terminal at its spaces, the
// continuation lines indented two columns deeper than the line: the screens
// write long sentences (reasons, warnings, help) that the renderer would cut
// at the right edge. A line with styling codes is left alone (its width is
// not its length).
func (m layoutModel) wrapView(v string) string {
	w, _ := m.dims()
	lines := strings.Split(v, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if len([]rune(line)) <= w || strings.Contains(line, "\x1b") {
			out = append(out, line)
			continue
		}
		body := strings.TrimLeft(line, " ")
		lead := line[:len(line)-len(body)]
		wrapped := wrapProse(body, w, lead+"  ")
		if len(wrapped) == 0 {
			out = append(out, line)
			continue
		}
		wrapped[0] = lead + strings.TrimPrefix(wrapped[0], lead+"  ")
		out = append(out, wrapped...)
	}
	return strings.Join(out, "\n")
}
