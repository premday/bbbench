package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

var (
	titleStyle        = lipgloss.NewStyle().MarginLeft(2)
	itemStyle         = lipgloss.NewStyle().PaddingLeft(4)
	selectedItemStyle = lipgloss.NewStyle().PaddingLeft(2).Foreground(lipgloss.Color("170"))
	paginationStyle   = list.DefaultStyles().PaginationStyle.PaddingLeft(4)
	helpStyle         = list.DefaultStyles().HelpStyle.PaddingLeft(4).PaddingBottom(1)
)

// driveItem implements list.Item interface for a drive.
type driveItem struct {
	drive DriveInfo
}

func (i driveItem) FilterValue() string {
	return i.drive.Device.Name
}

func (i driveItem) Title() string {
	return i.drive.Device.Name
}

func (i driveItem) Description() string {
	d := i.drive.Device
	typ := "SSD"
	if d.Rotational {
		typ = "HDD"
	}
	return fmt.Sprintf("%s | %s %s | %d GB | %s",
		d.Vendor, d.Model, d.Serial, d.CapacityGB(), typ)
}

// itemDelegate implements list.ItemDelegate for custom rendering.
type itemDelegate struct{}

func (d itemDelegate) Height() int                             { return 2 }
func (d itemDelegate) Spacing() int                            { return 1 }
func (d itemDelegate) Update(_ tea.Msg, _ *list.Model) tea.Cmd { return nil }
func (d itemDelegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	i, ok := listItem.(driveItem)
	if !ok {
		return
	}

	// Check if selected
	checkbox := "[ ]"
	if i.drive.Selected {
		checkbox = "[✓]"
	}

	str := fmt.Sprintf("%s %s", checkbox, i.Title())
	desc := i.Description()

	fn := itemStyle.Render
	if index == m.Index() {
		fn = func(s ...string) string {
			return selectedItemStyle.Render("> " + strings.Join(s, " "))
		}
	}

	fmt.Fprint(w, fn(str))
	fmt.Fprint(w, "\n")
	fmt.Fprint(w, itemStyle.Render("   "+desc))
}

// selectionModel is the bubbletea model for drive selection.
type selectionModel struct {
	list     list.Model
	drives   []DriveInfo
	quitting bool
	selected bool
}

func newSelectionModel(drives []DriveInfo) selectionModel {
	sorted := sortedDrives(drives)

	items := make([]list.Item, len(sorted))
	for i, d := range sorted {
		items[i] = driveItem{drive: d}
	}

	l := list.New(items, itemDelegate{}, 80, 20)
	l.Title = "Select drives to benchmark (Space to toggle, a=all, n=none, i=invert, Enter to confirm, q to quit)"
	l.SetShowStatusBar(false)
	l.SetFilteringEnabled(false)
	l.Styles.Title = titleStyle
	l.Styles.PaginationStyle = paginationStyle
	l.Styles.HelpStyle = helpStyle

	return selectionModel{
		list:   l,
		drives: sorted,
	}
}

func (m selectionModel) Init() tea.Cmd {
	return nil
}

func (m selectionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetWidth(msg.Width)
		m.list.SetHeight(msg.Height - 4)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit

		case " ": // Space to toggle selection
			if _, ok := m.list.SelectedItem().(driveItem); ok {
				idx := m.list.Index()
				m.drives[idx].Selected = !m.drives[idx].Selected
				m.list.SetItem(idx, driveItem{drive: m.drives[idx]})
			}
			return m, nil

		case "a": // Select all
			for i := range m.drives {
				m.drives[i].Selected = true
				m.list.SetItem(i, driveItem{drive: m.drives[i]})
			}
			return m, nil

		case "n": // Unselect all
			for i := range m.drives {
				m.drives[i].Selected = false
				m.list.SetItem(i, driveItem{drive: m.drives[i]})
			}
			return m, nil

		case "i": // Invert selection
			for i := range m.drives {
				m.drives[i].Selected = !m.drives[i].Selected
				m.list.SetItem(i, driveItem{drive: m.drives[i]})
			}
			return m, nil

		case "enter":
			m.selected = true
			m.quitting = true
			return m, tea.Quit
		}
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m selectionModel) View() string {
	if m.quitting {
		return ""
	}
	return "\n" + m.list.View()
}

// runDriveSelectionTUI launches the interactive TUI for drive selection.
func runDriveSelectionTUI(drives []DriveInfo) ([]DriveInfo, error) {
	m := newSelectionModel(drives)
	p := tea.NewProgram(m, tea.WithAltScreen())

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("tui error: %w", err)
	}

	result := finalModel.(selectionModel)
	if !result.selected {
		return nil, fmt.Errorf("selection cancelled")
	}

	// Return all selected drives (fio files will be auto-generated if missing)
	var selected []DriveInfo
	for _, d := range result.drives {
		if d.Selected {
			selected = append(selected, d)
		}
	}

	return selected, nil
}

// executionModel is a simple model for showing execution progress.
type executionModel struct {
	spinner  spinner.Model
	message  string
	quitting bool
}

func newExecutionModel() executionModel {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	return executionModel{
		spinner: s,
		message: "Initializing...",
	}
}

func (m executionModel) Init() tea.Cmd {
	return m.spinner.Tick
}

func (m executionModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit
		}
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m executionModel) View() string {
	if m.quitting {
		return ""
	}
	return fmt.Sprintf("\n\n   %s %s\n\n", m.spinner.View(), m.message)
}
