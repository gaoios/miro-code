package app

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ── Styles ──

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color("#FF79C6")).
			Padding(0, 1)

	searchStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#BD93F9")).
			Bold(true)

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F8F8F2")).
			Background(lipgloss.Color("#44475A")).
			Bold(true)

	normalStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#F8F8F2"))

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6272A4"))

	accentStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#50FA7B"))

	sizeStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFB86C")).
			Width(8).
			Align(lipgloss.Right)

	msgCountStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#8BE9FD")).
			Width(4).
			Align(lipgloss.Right)

	projectStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#BD93F9"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6272A4")).
			Padding(1, 0, 0, 1)

	statusStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#6272A4")).
			Padding(0, 1)

	detailBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("#44475A")).
			Padding(0, 1).
			MarginTop(1)
)

// ── TUI Model ──

type tuiMode int

const (
	modeList tuiMode = iota
	modeSearch
)

type tuiModel struct {
	allSessions []sessionInfo
	filtered    []sessionInfo
	cursor      int
	offset      int // viewport scroll offset
	height      int // terminal height
	width       int // terminal width
	mode        tuiMode
	search      textinput.Model
	query       string
	chosen      *sessionInfo // selected session to open
	quitting    bool
}

func newTUIModel(sessions []sessionInfo) tuiModel {
	// sort by modification time, descending
	sort.Slice(sessions, func(i, j int) bool {
		return sessions[i].ModTime.After(sessions[j].ModTime)
	})

	ti := textinput.New()
	ti.Placeholder = "search sessions..."
	ti.CharLimit = 100
	ti.Width = 60
	ti.PromptStyle = searchStyle
	ti.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#F8F8F2"))

	return tuiModel{
		allSessions: sessions,
		filtered:    sessions,
		search:      ti,
		height:      24,
		width:       120,
	}
}

func (m tuiModel) Init() tea.Cmd {
	return nil
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.height = msg.Height
		m.width = msg.Width
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeSearch:
			return m.updateSearch(msg)
		default:
			return m.updateList(msg)
		}
	}
	return m, nil
}

func (m tuiModel) updateList(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c", "esc":
		m.quitting = true
		return m, tea.Quit

	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
			if m.cursor < m.offset {
				m.offset = m.cursor
			}
		}

	case "down", "j":
		if m.cursor < len(m.filtered)-1 {
			m.cursor++
			visible := m.visibleRows()
			if m.cursor >= m.offset+visible {
				m.offset = m.cursor - visible + 1
			}
		}

	case "pgup":
		visible := m.visibleRows()
		m.cursor -= visible
		if m.cursor < 0 {
			m.cursor = 0
		}
		m.offset -= visible
		if m.offset < 0 {
			m.offset = 0
		}

	case "pgdown":
		visible := m.visibleRows()
		m.cursor += visible
		if m.cursor >= len(m.filtered) {
			m.cursor = len(m.filtered) - 1
		}
		m.offset += visible
		max := len(m.filtered) - visible
		if max < 0 {
			max = 0
		}
		if m.offset > max {
			m.offset = max
		}

	case "home", "g":
		m.cursor = 0
		m.offset = 0

	case "end", "G":
		m.cursor = len(m.filtered) - 1
		visible := m.visibleRows()
		m.offset = len(m.filtered) - visible
		if m.offset < 0 {
			m.offset = 0
		}

	case "/":
		m.mode = modeSearch
		m.search.Focus()
		return m, textinput.Blink

	case "enter":
		if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
			s := m.filtered[m.cursor]
			m.chosen = &s
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m tuiModel) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.search.Blur()
		return m, nil

	case "enter":
		m.mode = modeList
		m.search.Blur()
		m.query = m.search.Value()
		m.applyFilter()
		return m, nil
	}

	var cmd tea.Cmd
	m.search, cmd = m.search.Update(msg)
	// live filter
	m.query = m.search.Value()
	m.applyFilter()
	return m, cmd
}

func (m *tuiModel) applyFilter() {
	if m.query == "" {
		m.filtered = m.allSessions
	} else {
		q := strings.ToLower(m.query)
		var result []sessionInfo
		for _, s := range m.allSessions {
			if sessionMatchesQuery(s, q) {
				result = append(result, s)
			}
		}
		m.filtered = result
	}
	m.cursor = 0
	m.offset = 0
}

func (m tuiModel) detailHeight() int {
	if len(m.filtered) == 0 || m.cursor >= len(m.filtered) {
		return 0
	}
	s := m.filtered[m.cursor]
	lines := 2 // ID+project, time+size+messages always present
	if s.Model != "" {
		lines++
	}
	if s.Name != "" {
		lines++
	}
	if s.Summary != "" {
		lines++
	}
	lines += 3 // border top + bottom + margin top
	return lines
}

func (m tuiModel) visibleRows() int {
	// header(1) + search(1) + blank(1) + scroll indicator(1) + help(2) + detail
	reserved := 6 + m.detailHeight()
	v := m.height - reserved
	if v < 3 {
		v = 3
	}
	return v
}

func (m tuiModel) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// title bar
	total := len(m.allSessions)
	showing := len(m.filtered)
	title := titleStyle.Render(agentName + " Session Explorer")
	stats := dimStyle.Render(fmt.Sprintf(" %d/%d sessions", showing, total))
	b.WriteString(title + stats + "\n")

	// search bar
	if m.mode == modeSearch {
		b.WriteString(searchStyle.Render("/ ") + m.search.View() + "\n")
	} else if m.query != "" {
		b.WriteString(dimStyle.Render(fmt.Sprintf("  filter: %s  (/ to search, esc to clear)", m.query)) + "\n")
	} else {
		b.WriteString(dimStyle.Render("  / to search") + "\n")
	}

	b.WriteString("\n")

	// session list
	if len(m.filtered) == 0 {
		b.WriteString(dimStyle.Render("  no matching sessions\n"))
	} else {
		visible := m.visibleRows()
		end := m.offset + visible
		if end > len(m.filtered) {
			end = len(m.filtered)
		}

		for i := m.offset; i < end; i++ {
			s := m.filtered[i]
			isCursor := i == m.cursor

			line := m.renderRow(s, isCursor)
			b.WriteString(line + "\n")
		}

		// scrollbar indicator
		if len(m.filtered) > visible {
			pct := float64(m.offset) / float64(len(m.filtered)-visible) * 100
			b.WriteString(dimStyle.Render(fmt.Sprintf("  ── %.0f%% ──", pct)) + "\n")
		}
	}

	// detail panel for selected session
	if len(m.filtered) > 0 && m.cursor < len(m.filtered) {
		b.WriteString(m.renderDetail(m.filtered[m.cursor]))
	}

	// help
	help := "↑↓/jk navigate  enter open  / search  q quit  pgup/pgdn scroll  g/G top/bottom"
	b.WriteString(helpStyle.Render(help))

	return b.String()
}

func (m tuiModel) renderRow(s sessionInfo, selected bool) string {
	// indicator
	indicator := "  "
	if selected {
		indicator = "▸ "
	}

	// time
	age := time.Since(s.ModTime)
	var timeStr string
	switch {
	case age < time.Hour:
		timeStr = fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		timeStr = fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		timeStr = s.ModTime.Format("01/02 15:04")
	}
	timeStr = fmt.Sprintf("%-12s", timeStr)

	// size
	sz := humanSize(s.Size)

	// messages
	msgs := fmt.Sprintf("%d", s.Messages)

	// description
	desc := s.Title
	if desc == "" {
		desc = s.FirstMsg
	}
	if desc == "" && s.Summary != "" {
		desc = s.Summary
	}
	if desc == "" {
		desc = s.Name
	}
	// clean up control chars and newlines
	desc = strings.ReplaceAll(desc, "\n", " ")
	desc = strings.ReplaceAll(desc, "\r", "")

	// truncate description to fit
	maxDesc := m.width - 42 // indicator(2) + time(12) + size(8) + msgs(4) + padding(16)
	if maxDesc < 20 {
		maxDesc = 20
	}
	if len(desc) > maxDesc {
		desc = desc[:maxDesc-3] + "..."
	}

	if selected {
		line := fmt.Sprintf("%s%s %s %s  %s",
			indicator, timeStr, sz, msgs, desc)
		return selectedStyle.Render(line)
	}

	return fmt.Sprintf("%s%s %s %s  %s",
		accentStyle.Render(indicator),
		dimStyle.Render(timeStr),
		sizeStyle.Render(sz),
		msgCountStyle.Render(msgs),
		normalStyle.Render(desc),
	)
}

func (m tuiModel) renderDetail(s sessionInfo) string {
	var lines []string

	id := dimStyle.Render("ID: ") + normalStyle.Render(s.ID)
	proj := dimStyle.Render("Project: ") + projectStyle.Render(s.Project)
	lines = append(lines, id+"  "+proj)

	timeInfo := dimStyle.Render("Modified: ") + normalStyle.Render(s.ModTime.Format("2006-01-02 15:04:05"))
	if !s.StartTime.IsZero() {
		timeInfo += dimStyle.Render("  Started: ") + normalStyle.Render(s.StartTime.Format("15:04:05"))
	}
	sizeInfo := dimStyle.Render("  Size: ") + lipgloss.NewStyle().Foreground(lipgloss.Color("#FFB86C")).Render(humanSize(s.Size))
	msgInfo := dimStyle.Render("  Msgs: ") + lipgloss.NewStyle().Foreground(lipgloss.Color("#8BE9FD")).Render(fmt.Sprintf("%d", s.Messages))
	lines = append(lines, timeInfo+sizeInfo+msgInfo)

	if s.Model != "" {
		lines = append(lines, dimStyle.Render("Model: ")+normalStyle.Render(s.Model))
	}
	if s.Name != "" {
		lines = append(lines, dimStyle.Render("Name: ")+normalStyle.Render(s.Name))
	}
	if s.Summary != "" {
		sum := s.Summary
		if len(sum) > 120 {
			sum = sum[:117] + "..."
		}
		lines = append(lines, dimStyle.Render("Summary: ")+normalStyle.Render(sum))
	}

	content := strings.Join(lines, "\n")

	w := m.width - 4
	if w < 40 {
		w = 40
	}
	return detailBorder.Width(w).Render(content) + "\n"
}

// ── Entry points ──

func runSessionsTUI(sessions []sessionInfo) *sessionInfo {
	m := newTUIModel(sessions)
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		return nil
	}
	fm := finalModel.(tuiModel)
	return fm.chosen
}

func runSessionsTUIWithQuery(sessions []sessionInfo, query string) *sessionInfo {
	m := newTUIModel(sessions)
	m.query = query
	m.search.SetValue(query)
	m.applyFilter()
	p := tea.NewProgram(m, tea.WithAltScreen())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "TUI error: %v\n", err)
		return nil
	}
	fm := finalModel.(tuiModel)
	return fm.chosen
}
