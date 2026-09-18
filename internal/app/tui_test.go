package app

import (
	"strings"
	"testing"
	"time"
)

func TestNewTUIModel(t *testing.T) {
	sessions := []sessionInfo{
		{ID: "aaa", ModTime: time.Now().Add(-1 * time.Hour)},
		{ID: "bbb", ModTime: time.Now()},
		{ID: "ccc", ModTime: time.Now().Add(-2 * time.Hour)},
	}

	m := newTUIModel(sessions)

	// Should sort by mod time desc
	if m.filtered[0].ID != "bbb" {
		t.Errorf("first should be most recent, got %s", m.filtered[0].ID)
	}
	if m.filtered[2].ID != "ccc" {
		t.Errorf("last should be oldest, got %s", m.filtered[2].ID)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d, want 0", m.cursor)
	}
	if m.mode != modeList {
		t.Errorf("mode = %d, want modeList", m.mode)
	}
}

func TestTUIModel_ApplyFilter(t *testing.T) {
	sessions := []sessionInfo{
		{ID: "1", Title: "nginx config"},
		{ID: "2", Title: "gallery fix"},
		{ID: "3", Title: "nginx proxy"},
	}
	m := newTUIModel(sessions)

	m.query = "nginx"
	m.applyFilter()

	if len(m.filtered) != 2 {
		t.Errorf("expected 2 filtered, got %d", len(m.filtered))
	}
	for _, s := range m.filtered {
		if !strings.Contains(strings.ToLower(s.Title), "nginx") {
			t.Errorf("filtered session should match 'nginx': %s", s.Title)
		}
	}
}

func TestTUIModel_ApplyFilter_ClearsOnEmpty(t *testing.T) {
	sessions := []sessionInfo{
		{ID: "1", Title: "a"},
		{ID: "2", Title: "b"},
	}
	m := newTUIModel(sessions)

	m.query = "a"
	m.applyFilter()
	if len(m.filtered) != 1 {
		t.Errorf("expected 1, got %d", len(m.filtered))
	}

	m.query = ""
	m.applyFilter()
	if len(m.filtered) != 2 {
		t.Errorf("expected 2 (all), got %d", len(m.filtered))
	}
}

func TestTUIModel_ApplyFilter_ResetsCursor(t *testing.T) {
	sessions := make([]sessionInfo, 10)
	for i := range sessions {
		sessions[i] = sessionInfo{ID: string(rune('a' + i)), Title: "session"}
	}
	m := newTUIModel(sessions)
	m.cursor = 5

	m.query = "session"
	m.applyFilter()

	if m.cursor != 0 {
		t.Errorf("cursor should reset to 0, got %d", m.cursor)
	}
	if m.offset != 0 {
		t.Errorf("offset should reset to 0, got %d", m.offset)
	}
}

func TestTUIModel_VisibleRows(t *testing.T) {
	m := newTUIModel(nil)
	m.height = 40

	rows := m.visibleRows()
	if rows < 3 {
		t.Errorf("visibleRows = %d, should be at least 3", rows)
	}
	if rows > 40 {
		t.Errorf("visibleRows = %d, should not exceed terminal height", rows)
	}
}

func TestTUIModel_VisibleRows_SmallTerminal(t *testing.T) {
	m := newTUIModel(nil)
	m.height = 5

	rows := m.visibleRows()
	if rows < 3 {
		t.Errorf("visibleRows = %d, minimum should be 3", rows)
	}
}

func TestTUIModel_DetailHeight_NoSessions(t *testing.T) {
	m := newTUIModel(nil)
	h := m.detailHeight()
	if h != 0 {
		t.Errorf("detailHeight with no sessions = %d, want 0", h)
	}
}

func TestTUIModel_DetailHeight_WithModel(t *testing.T) {
	sessions := []sessionInfo{
		{ID: "1", Model: "claude-opus-4-6"},
	}
	m := newTUIModel(sessions)
	h := m.detailHeight()
	// Should include model line
	if h < 5 { // base(2) + model(1) + border(3)
		t.Errorf("detailHeight with model = %d, expected >= 5", h)
	}
}

func TestTUIModel_RenderRow(t *testing.T) {
	s := sessionInfo{
		ID:       "abc12345",
		Title:    "Test Session",
		Size:     2048,
		Messages: 10,
		ModTime:  time.Now().Add(-30 * time.Minute),
	}
	m := newTUIModel([]sessionInfo{s})
	m.width = 120

	row := m.renderRow(s, false)
	if row == "" {
		t.Error("renderRow should not be empty")
	}

	selectedRow := m.renderRow(s, true)
	if selectedRow == "" {
		t.Error("selected renderRow should not be empty")
	}
}

func TestTUIModel_RenderRow_LongDescription(t *testing.T) {
	s := sessionInfo{
		ID:      "abc12345",
		Title:   strings.Repeat("long title ", 20),
		Size:    1024,
		ModTime: time.Now(),
	}
	m := newTUIModel([]sessionInfo{s})
	m.width = 80

	row := m.renderRow(s, false)
	// Should be truncated and not crash
	if row == "" {
		t.Error("renderRow should handle long descriptions")
	}
}

func TestTUIModel_RenderDetail(t *testing.T) {
	s := sessionInfo{
		ID:        "abc12345-6789",
		Title:     "Test",
		Project:   "~/test/project",
		Size:      4096,
		Messages:  20,
		Model:     "claude-opus-4-6",
		Name:      "weiran-0405-1234",
		Summary:   "This is a summary",
		ModTime:   time.Now(),
		StartTime: time.Now().Add(-1 * time.Hour),
	}
	m := newTUIModel([]sessionInfo{s})
	m.width = 120

	detail := m.renderDetail(s)
	if detail == "" {
		t.Error("renderDetail should not be empty")
	}
}

func TestTUIModel_View(t *testing.T) {
	sessions := []sessionInfo{
		{ID: "aaa", Title: "First", ModTime: time.Now(), Size: 1024},
		{ID: "bbb", Title: "Second", ModTime: time.Now(), Size: 2048},
	}
	m := newTUIModel(sessions)
	m.height = 30
	m.width = 120

	view := m.View()
	if view == "" {
		t.Error("View should not be empty")
	}
	if !strings.Contains(view, "Session Explorer") {
		t.Error("View should contain title")
	}
}

func TestTUIModel_View_Quitting(t *testing.T) {
	m := newTUIModel(nil)
	m.quitting = true

	view := m.View()
	if view != "" {
		t.Errorf("quitting view should be empty, got %q", view)
	}
}

func TestTUIModel_View_Empty(t *testing.T) {
	m := newTUIModel(nil)
	m.height = 30
	m.width = 120

	view := m.View()
	if !strings.Contains(view, "no matching sessions") {
		t.Error("empty view should show no-match message")
	}
}

func TestTUIModel_Init(t *testing.T) {
	m := newTUIModel(nil)
	cmd := m.Init()
	if cmd != nil {
		t.Error("Init should return nil cmd")
	}
}
