package tui

import "github.com/charmbracelet/lipgloss"

var (
	accent  = lipgloss.AdaptiveColor{Light: "#9A5B13", Dark: "#E8A84A"}
	muted   = lipgloss.AdaptiveColor{Light: "#6B6B6B", Dark: "#8A8A8A"}
	good    = lipgloss.AdaptiveColor{Light: "#2E7D32", Dark: "#7BC67E"}
	bad     = lipgloss.AdaptiveColor{Light: "#C62828", Dark: "#EF7A7A"}
	archCol = lipgloss.AdaptiveColor{Light: "#6A3FA0", Dark: "#C3A3F0"}
	dirCol  = lipgloss.AdaptiveColor{Light: "#1565C0", Dark: "#82B4F0"}
	selBg   = lipgloss.AdaptiveColor{Light: "#F1E3CC", Dark: "#3A2F1E"}

	titleStyle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
	pathStyle    = lipgloss.NewStyle().Foreground(muted)
	mutedStyle   = lipgloss.NewStyle().Foreground(muted)
	goodStyle    = lipgloss.NewStyle().Foreground(good)
	badStyle     = lipgloss.NewStyle().Foreground(bad)
	dirStyle     = lipgloss.NewStyle().Foreground(dirCol).Bold(true)
	archStyle    = lipgloss.NewStyle().Foreground(archCol)
	cursorStyle  = lipgloss.NewStyle().Background(selBg)
	markStyle    = lipgloss.NewStyle().Foreground(accent).Bold(true)
	keyStyle     = lipgloss.NewStyle().Foreground(accent)
	labelStyle   = lipgloss.NewStyle().Width(26)
	focusLabel   = lipgloss.NewStyle().Width(26).Foreground(accent).Bold(true)
	buttonStyle  = lipgloss.NewStyle().Padding(0, 2).Border(lipgloss.RoundedBorder()).BorderForeground(muted)
	buttonActive = lipgloss.NewStyle().Padding(0, 2).Border(lipgloss.RoundedBorder()).BorderForeground(accent).Foreground(accent).Bold(true)
)
