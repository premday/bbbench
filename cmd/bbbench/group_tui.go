package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"bbbench/internal/diskinfo"
	"bbbench/internal/rungroup"
	"bbbench/internal/settings"
)

// targetsFor describes the drives as targets of a setting. The interface is
// read from sysfs only: no command runs and no drive is touched.
func targetsFor(drives []DriveInfo, opts diskinfo.Options) map[string]settings.Target {
	targets := make(map[string]settings.Target, len(drives))
	for _, d := range drives {
		targets[d.Device.Name] = settings.Target{
			Name:       d.Device.Name,
			Path:       d.Device.Path,
			Serial:     d.Device.Serial,
			Model:      d.Device.Model,
			Vendor:     d.Device.Vendor,
			Interface:  diskinfo.InterfaceOf(d.Device.Name, opts),
			Rotational: d.Device.Rotational,
		}
	}
	return targets
}

// firmwareFor reads the firmware of each drive itself, from sysfs only.
func firmwareFor(drives []DriveInfo, opts diskinfo.Options) map[string]string {
	fw := make(map[string]string, len(drives))
	for _, d := range drives {
		if v := diskinfo.DriveFirmware(d.Device.Name, opts); v != "" {
			fw[d.Device.Name] = v
		}
	}
	return fw
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---------------------------------------------------------------------------
// Mode: single run or setting test

// modeChoice is what the user wants to do; Setting nil is a single run.
type modeChoice struct{ Setting settings.Setting }

type modeEntry struct {
	setting settings.Setting
	applies int
	reasons []string // "drive: reason" for the drives it does not apply to
}

// modeModel asks single run or setting test, then which setting.
type modeModel struct {
	entries   []modeEntry
	total     int
	inSetting bool // false: the single run / setting test question
	cursor    int
	msg       string
	choice    modeChoice
	done      bool
	cancelled bool
}

func newModeModel(available []settings.Setting, drives []DriveInfo, targets map[string]settings.Target) modeModel {
	m := modeModel{total: len(drives)}
	// Settings of values and of variants (layouts) alike; each is set up by
	// its own screens afterwards.
	for _, s := range available {
		e := modeEntry{setting: s}
		for _, d := range drives {
			name := d.Device.Name
			if ok, why := s.Applicable(targets[name]); ok {
				e.applies++
			} else {
				e.reasons = append(e.reasons, name+": "+why)
			}
		}
		m.entries = append(m.entries, e)
	}
	return m
}

func (m modeModel) Init() tea.Cmd { return nil }

func (m modeModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	n := 2
	if m.inSetting {
		n = len(m.entries)
	}
	m.msg = ""
	switch k.String() {
	case "ctrl+c", "q":
		m.cancelled = true
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < n-1 {
			m.cursor++
		}
	case "esc":
		if m.inSetting {
			m.inSetting, m.cursor = false, 1
			return m, nil
		}
		m.cancelled = true
		return m, tea.Quit
	case "enter":
		if !m.inSetting {
			if m.cursor == 0 {
				m.done = true
				return m, tea.Quit
			}
			if len(m.entries) == 0 {
				m.msg = "No setting can be tested."
				return m, nil
			}
			m.inSetting, m.cursor = true, 0
			return m, nil
		}
		e := m.entries[m.cursor]
		if e.applies == 0 {
			m.msg = "This setting applies to none of the drives."
			return m, nil
		}
		m.choice = modeChoice{Setting: e.setting}
		m.done = true
		return m, tea.Quit
	}
	return m, nil
}

func (m modeModel) View() string {
	if m.done || m.cancelled {
		return ""
	}
	var b strings.Builder
	line := func(i int, text string) {
		c := "  "
		if i == m.cursor {
			c = "> "
		}
		b.WriteString("  " + c + text + "\n")
	}
	if !m.inSetting {
		b.WriteString("\n  What do you want to do? (up/down, Enter, q to quit)\n\n")
		line(0, "Single run: benchmark the drives you select, as they are")
		line(1, "Setting test: run the whole test plan once per value of a setting")
	} else {
		b.WriteString("\n  Which setting? (up/down, Enter, Esc to go back, q to quit)\n\n")
		for i, e := range m.entries {
			line(i, fmt.Sprintf("%s: applies to %d of %d %s", e.setting.Name(), e.applies, m.total, plural(m.total, "drive", "drives")))
			for _, r := range e.reasons {
				b.WriteString("        not " + r + "\n")
			}
		}
	}
	if m.msg != "" {
		b.WriteString("\n  " + m.msg + "\n")
	}
	return b.String()
}

// runModeTUI asks whether to do a single run or a setting test, and for a
// setting test which setting. It runs before the drive selection.
func runModeTUI(available []settings.Setting, drives []DriveInfo, targets map[string]settings.Target) (modeChoice, error) {
	final, err := runTea(newModeModel(available, drives, targets))
	if err != nil {
		return modeChoice{}, err
	}
	m := final.(modeModel)
	if m.cancelled || !m.done {
		return modeChoice{}, errors.New("cancelled")
	}
	return m.choice, nil
}

// ---------------------------------------------------------------------------
// Group setup: values, strategy, confirmation

// groupChoice is what the user confirmed for a setting test. Drives are the
// drives the setting applies to; Excluded maps the others to the reason.
// For a layout group, Variants are the layouts in order, Values their
// values, Drives the union of their disks in order of first use and the
// strategy is sequential.
type groupChoice struct {
	Setting  settings.Setting
	Values   []settings.Value
	Strategy rungroup.Strategy
	Drives   []DriveInfo
	Excluded map[string]string
	Variants []settings.Variant
}

type setupStep int

const (
	stepValues setupStep = iota
	stepStrategy
	stepConfirm
)

type setupModel struct {
	setting  settings.Setting
	firmware map[string]string
	drives   []DriveInfo
	excluded map[string]string
	exOrder  []string // excluded device names in selection order

	values []settings.Value
	ticked []bool
	cursor int
	step   setupStep
	msg    string

	options   []rungroup.Strategy
	notOffer  string // why parallel is not offered; "" when it is
	strategy  rungroup.Strategy
	firmwares []string // warnings: same model, different firmware

	stopped   bool // no selected drive is applicable
	done      bool
	cancelled bool
	result    *groupChoice
}

func newSetupModel(s settings.Setting, selected []DriveInfo, targets map[string]settings.Target, firmware map[string]string) setupModel {
	m := setupModel{setting: s, firmware: firmware, excluded: map[string]string{}, values: s.Values(), strategy: rungroup.Sequential}
	for _, d := range selected {
		name := d.Device.Name
		if ok, why := s.Applicable(targets[name]); ok {
			m.drives = append(m.drives, d)
		} else {
			m.excluded[name] = why
			m.exOrder = append(m.exOrder, name)
		}
	}
	m.ticked = make([]bool, len(m.values))
	for i := range m.ticked {
		m.ticked[i] = true
	}
	m.stopped = len(m.drives) == 0
	m.firmwares = firmwareWarnings(driveRefs(m.drives, firmware))
	return m
}

// firmwareWarnings lists the classes of identical drives that run different
// firmware: a warning, not a blocker.
func firmwareWarnings(refs []rungroup.DriveRef) []string {
	type key struct {
		vendor, model string
		capacity      uint64
	}
	byClass := map[key]map[string][]string{} // class -> firmware -> drives
	var order []key
	for _, r := range refs {
		if r.Firmware == "" {
			continue
		}
		k := key{r.Vendor, r.Model, r.CapacityBytes}
		if byClass[k] == nil {
			byClass[k] = map[string][]string{}
			order = append(order, k)
		}
		byClass[k][r.Firmware] = append(byClass[k][r.Firmware], r.Name)
	}
	var out []string
	for _, k := range order {
		fw := byClass[k]
		if len(fw) < 2 {
			continue
		}
		var parts []string
		for f, names := range fw {
			sort.Strings(names)
			parts = append(parts, fmt.Sprintf("%s on %s", f, strings.Join(names, ", ")))
		}
		sort.Strings(parts)
		out = append(out, fmt.Sprintf("Warning: %s %s drives have different firmware (%s); drive-to-drive differences may show in the results.", k.vendor, k.model, strings.Join(parts, "; ")))
	}
	return out
}

func (m setupModel) Init() tea.Cmd { return nil }

func (m setupModel) chosenValues() []settings.Value {
	var out []settings.Value
	for i, v := range m.values {
		if m.ticked[i] {
			out = append(out, v)
		}
	}
	return out
}

func (m setupModel) valueIDs() []string {
	var ids []string
	for _, v := range m.chosenValues() {
		ids = append(ids, v.ID)
	}
	return ids
}

// plans returns the runs of the chosen strategy.
func (m setupModel) plans() ([]rungroup.RunPlan, error) {
	return rungroup.PlanRuns(driveRefs(m.drives, m.firmware), m.valueIDs(), m.strategy)
}

// enterStrategy works out which strategies are offered for the ticked values.
func (m *setupModel) enterStrategy() {
	m.options = []rungroup.Strategy{rungroup.Sequential}
	m.notOffer = ""
	if ok, why := rungroup.ParallelPossible(driveRefs(m.drives, m.firmware), len(m.valueIDs())); ok {
		m.options = append(m.options, rungroup.Parallel, rungroup.ParallelSwap)
	} else {
		m.notOffer = why
	}
	m.cursor, m.strategy, m.step = 0, rungroup.Sequential, stepStrategy
}

func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return m, nil
	}
	m.msg = ""
	key := k.String()
	if m.stopped {
		m.cancelled = true
		return m, tea.Quit
	}
	switch key {
	case "ctrl+c", "q":
		m.cancelled = true
		return m, tea.Quit
	case "esc":
		switch m.step {
		case stepValues:
			m.cancelled = true
			return m, tea.Quit
		case stepStrategy:
			m.step, m.cursor = stepValues, 0
		case stepConfirm:
			m.enterStrategy()
		}
		return m, nil
	}
	switch m.step {
	case stepValues:
		switch key {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.values)-1 {
				m.cursor++
			}
		case " ":
			m.ticked[m.cursor] = !m.ticked[m.cursor]
		case "enter":
			if len(m.valueIDs()) < 2 {
				m.msg = "Tick at least two values to compare."
				return m, nil
			}
			m.enterStrategy()
		}
	case stepStrategy:
		switch key {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}
		case "down", "j":
			if m.cursor < len(m.options)-1 {
				m.cursor++
			}
		case "enter":
			m.strategy, m.step = m.options[m.cursor], stepConfirm
		}
	case stepConfirm:
		if key == "enter" {
			m.result = &groupChoice{Setting: m.setting, Values: m.chosenValues(), Strategy: m.strategy, Drives: m.drives, Excluded: m.excluded}
			m.done = true
			return m, tea.Quit
		}
	}
	return m, nil
}

// outcome is the confirmed choice, or why there is none.
func (m setupModel) outcome() (*groupChoice, error) {
	if m.stopped {
		return nil, errors.New(m.stopReason())
	}
	if m.cancelled || m.result == nil {
		return nil, errors.New("cancelled")
	}
	return m.result, nil
}

func (m setupModel) stopReason() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s applies to none of the selected drives:", m.setting.Name())
	for _, n := range m.exOrder {
		fmt.Fprintf(&b, "\n  %s: %s", n, m.excluded[n])
	}
	return b.String()
}

func (m setupModel) View() string {
	if m.done || m.cancelled {
		return ""
	}
	if m.stopped {
		return "\n  " + strings.ReplaceAll(m.stopReason(), "\n", "\n  ") + "\n\n  Press any key to quit.\n"
	}
	var b strings.Builder
	cur := func(i int) string {
		if i == m.cursor {
			return "> "
		}
		return "  "
	}
	switch m.step {
	case stepValues:
		fmt.Fprintf(&b, "\n  Values of %s to test (Space to tick, Enter to continue, Esc to quit)\n\n", m.setting.Name())
		for i, v := range m.values {
			box := "[ ]"
			if m.ticked[i] {
				box = "[x]"
			}
			fmt.Fprintf(&b, "  %s%s %s\n", cur(i), box, v.Label)
		}
	case stepStrategy:
		b.WriteString("\n  How are the values spread over the drives? (up/down, Enter, Esc to go back)\n\n")
		for i, o := range m.options {
			fmt.Fprintf(&b, "  %s%s\n", cur(i), strategyText(o))
		}
		if m.notOffer != "" {
			fmt.Fprintf(&b, "\n  parallel is not offered: %s\n", m.notOffer)
		}
		m.warnings(&b)
	case stepConfirm:
		m.confirmView(&b)
	}
	if m.msg != "" {
		b.WriteString("\n  " + m.msg + "\n")
	}
	return b.String()
}

func strategyText(s rungroup.Strategy) string {
	switch s {
	case rungroup.Parallel:
		return "parallel: identical drives are divided among the values, all values run at the same time"
	case rungroup.ParallelSwap:
		return "parallel with swap rounds: parallel, then the values rotate over the drives, so each is measured on every drive subset"
	}
	return "one value after the other (default): every drive gets the same value in a run"
}

func (m setupModel) warnings(b *strings.Builder) {
	for _, w := range m.firmwares {
		b.WriteString("\n  " + w + "\n")
	}
}

func (m setupModel) confirmView(b *strings.Builder) {
	b.WriteString("\n  Confirm the setting test (Enter to start, Esc to go back, q to quit)\n\n")
	fmt.Fprintf(b, "  Setting:   %s\n", m.setting.Name())
	var labels []string
	for _, v := range m.chosenValues() {
		labels = append(labels, v.Label)
	}
	fmt.Fprintf(b, "  Values:    %s\n", strings.Join(labels, ", "))
	fmt.Fprintf(b, "  Strategy:  %s\n", m.strategy)
	b.WriteString("  Will be changed on:\n")
	for _, d := range m.drives {
		fmt.Fprintf(b, "    %s  %s %s (S/N %s)\n", d.Device.Name, d.Device.Vendor, d.Device.Model, d.Device.Serial)
	}
	if len(m.exOrder) > 0 {
		b.WriteString("  Not changed (setting does not apply):\n")
		for _, n := range m.exOrder {
			fmt.Fprintf(b, "    %s: %s\n", n, m.excluded[n])
		}
	}
	plans, err := m.plans()
	if err != nil {
		fmt.Fprintf(b, "\n  Cannot plan the runs: %v\n", err)
		return
	}
	label := map[string]string{}
	for _, v := range m.chosenValues() {
		label[v.ID] = v.Label
	}
	b.WriteString("\n  Which value goes to which drive:\n")
	for i, p := range plans {
		var parts []string
		for _, v := range p.Variants {
			parts = append(parts, fmt.Sprintf("%s -> %s", label[v.Value], strings.Join(v.Drives, ", ")))
		}
		fmt.Fprintf(b, "    Run %d: %s\n", i+1, strings.Join(parts, "; "))
	}
	per, unknown, missing := estimateRun(m.drives)
	fmt.Fprintf(b, "\n  %d %s of the full test plan", len(plans), plural(len(plans), "run", "runs"))
	switch {
	case missing > 0 && per == 0:
		b.WriteString("; estimated time unknown until the fio files are generated\n")
	default:
		fmt.Fprintf(b, "; estimated time %s", (time.Duration(len(plans)) * per).Round(time.Second))
		if unknown > 0 {
			fmt.Fprintf(b, " or more (%d %s of unknown length not counted)", unknown, plural(unknown, "phase", "phases"))
		}
		b.WriteString("\n")
	}
	m.warnings(b)
	b.WriteString("\n  The original values are read first and restored when the group ends,\n  also after an error or Ctrl+C. Nothing is changed before you press Enter.\n")
}

// estimateRun estimates one run of the test plan: for each drive the sum of
// its phases with a known length; the longest drive counts. unknown is the
// most phases of unknown length on one drive; missing counts drives whose fio
// file could not be read (it may not be generated yet).
func estimateRun(drives []DriveInfo) (per time.Duration, unknown, missing int) {
	for _, d := range drives {
		w, err := parseFioFile(d.FioFilePath)
		if err != nil {
			missing++
			continue
		}
		global := parseFioOptions(w.GlobalSection)
		var sum time.Duration
		u := 0
		for _, phase := range w.Phases {
			sec, ok, _ := phaseDuration(global, phase)
			if !ok {
				u++
				continue
			}
			sum += time.Duration(sec) * time.Second
		}
		per = max(per, sum)
		unknown = max(unknown, u)
	}
	return per, unknown, missing
}

// runGroupSetupTUI asks, after the drive selection, which values to test and
// how to spread them, and the confirmation. Nothing is applied here.
func runGroupSetupTUI(setting settings.Setting, selected []DriveInfo, targets map[string]settings.Target) (*groupChoice, error) {
	m := newSetupModel(setting, selected, targets, firmwareFor(selected, diskinfo.Options{}))
	final, err := runTea(m)
	if err != nil {
		return nil, err
	}
	return final.(setupModel).outcome()
}

// runTea runs a model on the alternate screen and returns the final model.
func runTea(m tea.Model) (tea.Model, error) {
	final, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	if err != nil {
		return nil, fmt.Errorf("tui error: %w", err)
	}
	return final, nil
}
