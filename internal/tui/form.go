package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type fieldKind int

const (
	fChoice fieldKind = iota
	fToggle
	fText
	fSlider
)

// field is one row of a form.
type field struct {
	key     string
	label   string
	kind    fieldKind
	choices []string
	idx     int
	on      bool
	input   textinput.Model
	val     int
	min     int
	max     int
	names   []string // slider labels
	hint    string
	visible func(*form) bool
}

func (f *field) value() string {
	switch f.kind {
	case fChoice:
		return f.choices[f.idx]
	case fText:
		return f.input.Value()
	case fToggle:
		return fmt.Sprint(f.on)
	}
	return fmt.Sprint(f.val)
}

// form is a list of fields followed by buttons.
type form struct {
	title   string
	sub     string
	fields  []*field
	buttons []string
	focus   int // index into visible fields, then buttons
	err     string
}

func (fm *form) get(key string) *field {
	for _, f := range fm.fields {
		if f.key == key {
			return f
		}
	}
	panic("no field " + key)
}

func (fm *form) visibleFields() []*field {
	var out []*field
	for _, f := range fm.fields {
		if f.visible == nil || f.visible(fm) {
			out = append(out, f)
		}
	}
	return out
}

func (fm *form) count() int { return len(fm.visibleFields()) + len(fm.buttons) }

func (fm *form) current() (*field, int) {
	vf := fm.visibleFields()
	if fm.focus < len(vf) {
		return vf[fm.focus], -1
	}
	return nil, fm.focus - len(vf)
}

func (fm *form) setFocus(i int) {
	n := fm.count()
	fm.focus = (i%n + n) % n
	for _, f := range fm.fields {
		if f.kind == fText {
			f.input.Blur()
		}
	}
	if f, _ := fm.current(); f != nil && f.kind == fText {
		f.input.Focus()
	}
}

// update handles a key. It returns the pressed button index, or -1.
func (fm *form) update(msg tea.KeyMsg) (int, tea.Cmd) {
	f, btn := fm.current()
	switch msg.String() {
	case "up", "shift+tab":
		fm.setFocus(fm.focus - 1)
		return -1, nil
	case "down", "tab":
		fm.setFocus(fm.focus + 1)
		return -1, nil
	case "enter":
		if btn >= 0 {
			return btn, nil
		}
		fm.setFocus(fm.focus + 1)
		return -1, nil
	}
	if btn >= 0 {
		switch msg.String() {
		case "left", "h":
			if btn > 0 {
				fm.setFocus(fm.focus - 1)
			}
		case "right", "l":
			if btn < len(fm.buttons)-1 {
				fm.setFocus(fm.focus + 1)
			}
		}
		return -1, nil
	}
	switch f.kind {
	case fText:
		var cmd tea.Cmd
		f.input, cmd = f.input.Update(msg)
		return -1, cmd
	case fChoice:
		switch msg.String() {
		case "left", "h":
			f.idx = (f.idx - 1 + len(f.choices)) % len(f.choices)
		case "right", "l", " ":
			f.idx = (f.idx + 1) % len(f.choices)
		}
	case fToggle:
		switch msg.String() {
		case " ", "left", "right", "h", "l", "x":
			f.on = !f.on
		}
	case fSlider:
		switch msg.String() {
		case "left", "h":
			if f.val > f.min {
				f.val--
			}
		case "right", "l":
			if f.val < f.max {
				f.val++
			}
		default:
			if s := msg.String(); len(s) == 1 && s[0] >= '0' && s[0] <= '9' {
				v := int(s[0] - '0')
				if v >= f.min && v <= f.max {
					f.val = v
				}
			}
		}
	}
	return -1, nil
}

func (fm *form) view(width int) string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(fm.title) + "\n")
	if fm.sub != "" {
		b.WriteString(pathStyle.Render(fm.sub) + "\n")
	}
	b.WriteString("\n")
	cur, btn := fm.current()
	for _, f := range fm.visibleFields() {
		focused := f == cur
		lbl := labelStyle.Render(f.label)
		if focused {
			lbl = focusLabel.Render("› " + f.label)
		}
		var v string
		switch f.kind {
		case fChoice:
			v = f.choices[f.idx]
			if focused {
				v = keyStyle.Render("‹ ") + v + keyStyle.Render(" ›")
			}
		case fToggle:
			v = "[ ]"
			if f.on {
				v = markStyle.Render("[x]")
			}
		case fText:
			v = f.input.View()
		case fSlider:
			bar := strings.Repeat("■", f.val-f.min) + mutedStyle.Render(strings.Repeat("·", f.max-f.val))
			name := ""
			if f.names != nil {
				name = " " + f.names[f.val]
			}
			v = fmt.Sprintf("%s %d%s", bar, f.val, name)
		}
		line := lbl + v
		if focused && f.hint != "" {
			line += "  " + mutedStyle.Render(f.hint)
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\n")
	var bs []string
	for i, name := range fm.buttons {
		if i == btn {
			bs = append(bs, buttonActive.Render(name))
		} else {
			bs = append(bs, buttonStyle.Render(name))
		}
	}
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, bs...) + "\n")
	if fm.err != "" {
		b.WriteString("\n" + badStyle.Render(fm.err) + "\n")
	}
	b.WriteString("\n" + mutedStyle.Render("↑↓ move · ←→ change · space toggle · enter next/press · esc back"))
	return b.String()
}

func newText(placeholder, value string, secret bool) textinput.Model {
	t := textinput.New()
	t.Placeholder = placeholder
	t.SetValue(value)
	t.Prompt = ""
	t.CharLimit = 4096
	t.Width = 40
	if secret {
		t.EchoMode = textinput.EchoPassword
		t.EchoCharacter = '•'
	}
	return t
}
