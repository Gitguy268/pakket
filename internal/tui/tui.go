// Package tui is pakket's interactive file browser, option forms and job queue.
package tui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Gitguy268/pakket/internal/config"
	"github.com/Gitguy268/pakket/internal/engine"
)

type screen int

const (
	scBrowse screen = iota
	scCompress
	scExtract
	scQueue
	scContents
	scHelp
	scPassword
)

type entry struct {
	name    string
	path    string
	dir     bool
	size    int64
	archive string // format name when it is an archive
}

type model struct {
	cfg    config.Config
	screen screen
	back   screen
	width  int
	height int

	cwd      string
	entries  []entry
	cursor   int
	offset   int
	selected map[string]bool
	filter   string
	typing   bool // filter input active
	status   string

	form    *form
	targets []string

	jobs    []*job
	qcursor int
	events  chan tea.Msg
	running bool

	contents    []engine.Entry
	contentsOf  string
	contentsErr string
	coffset     int

	pwForm *form
	pwJob  *job
	pwList string // archive waiting for a password to list contents
}

// Run starts the TUI in the current folder.
func Run(cfg config.Config) error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	m := &model{cfg: cfg, cwd: wd, selected: map[string]bool{}, events: make(chan tea.Msg, 64)}
	m.load()
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	for _, j := range m.jobs {
		if j.cancel != nil {
			j.cancel()
		}
	}
	return err
}

func (m *model) Init() tea.Cmd { return waitEvent(m.events) }

func waitEvent(ch chan tea.Msg) tea.Cmd {
	return func() tea.Msg { return <-ch }
}

func (m *model) load() {
	ents, err := os.ReadDir(m.cwd)
	if err != nil {
		m.status = err.Error()
		return
	}
	m.entries = m.entries[:0]
	for _, e := range ents {
		if !m.cfg.ShowHidden && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if m.filter != "" && !strings.Contains(strings.ToLower(e.Name()), strings.ToLower(m.filter)) {
			continue
		}
		p := filepath.Join(m.cwd, e.Name())
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		en := entry{name: e.Name(), path: p, dir: info.IsDir(), size: info.Size()}
		if !en.dir && engine.IsArchive(p) {
			if f, err := engine.Detect(p); err == nil {
				en.archive = f.Name
			}
		}
		m.entries = append(m.entries, en)
	}
	sort.SliceStable(m.entries, func(i, j int) bool {
		if m.entries[i].dir != m.entries[j].dir {
			return m.entries[i].dir
		}
		return strings.ToLower(m.entries[i].name) < strings.ToLower(m.entries[j].name)
	})
	if m.cursor >= len(m.entries) {
		m.cursor = max(0, len(m.entries)-1)
	}
}

func (m *model) listHeight() int { return max(3, m.height-6) }

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case progressMsg, jobDoneMsg, contentsMsg:
		cmd := m.handleEvent(msg)
		return m, tea.Batch(cmd, waitEvent(m.events))
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		// Keys typed quickly can arrive as one message; outside text fields
		// they are commands, so handle them one by one.
		textual := m.screen == scCompress || m.screen == scExtract || m.screen == scPassword || (m.screen == scBrowse && m.typing)
		if msg.Type == tea.KeyRunes && len(msg.Runes) > 1 && !textual {
			var cmds []tea.Cmd
			for _, r := range msg.Runes {
				_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
				cmds = append(cmds, cmd)
			}
			return m, tea.Batch(cmds...)
		}
		switch m.screen {
		case scBrowse:
			return m, m.updateBrowse(msg)
		case scCompress, scExtract:
			return m, m.updateForm(msg)
		case scQueue:
			return m, m.updateQueue(msg)
		case scContents:
			return m, m.updateContents(msg)
		case scPassword:
			return m, m.updatePassword(msg)
		case scHelp:
			m.screen = m.back
			return m, nil
		}
	}
	return m, nil
}

func (m *model) current() *entry {
	if m.cursor < len(m.entries) {
		return &m.entries[m.cursor]
	}
	return nil
}

// picked returns the selected paths, or the one under the cursor.
func (m *model) picked() []string {
	var out []string
	for _, e := range m.entries {
		if m.selected[e.path] {
			out = append(out, e.path)
		}
	}
	for p := range m.selected {
		if filepath.Dir(p) != m.cwd { // selected in another folder
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		if e := m.current(); e != nil {
			out = []string{e.path}
		}
	}
	sort.Strings(out)
	return out
}

func (m *model) updateBrowse(k tea.KeyMsg) tea.Cmd {
	if m.typing {
		switch k.Type {
		case tea.KeyEsc:
			m.typing, m.filter = false, ""
		case tea.KeyEnter:
			m.typing = false
		case tea.KeyBackspace:
			if m.filter != "" {
				m.filter = m.filter[:len(m.filter)-1]
			}
		case tea.KeyRunes, tea.KeySpace:
			m.filter += string(k.Runes)
		}
		m.cursor = 0
		m.load()
		return nil
	}
	m.status = ""
	switch k.String() {
	case "q":
		return tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.entries)-1 {
			m.cursor++
		}
	case "pgup":
		m.cursor = max(0, m.cursor-m.listHeight())
	case "pgdown":
		m.cursor = min(len(m.entries)-1, m.cursor+m.listHeight())
	case "g", "home":
		m.cursor = 0
	case "G", "end":
		m.cursor = max(0, len(m.entries)-1)
	case "enter", "right", "l":
		e := m.current()
		if e == nil {
			return nil
		}
		if e.dir {
			m.cwd, m.cursor, m.offset, m.filter = e.path, 0, 0, ""
			m.load()
		} else if e.archive != "" {
			return m.openContents(e.path, "")
		}
	case "left", "h", "backspace":
		parent := filepath.Dir(m.cwd)
		if parent != m.cwd {
			from := filepath.Base(m.cwd)
			m.cwd, m.offset, m.filter = parent, 0, ""
			m.load()
			for i, e := range m.entries {
				if e.name == from {
					m.cursor = i
				}
			}
		}
	case "~":
		if home, err := os.UserHomeDir(); err == nil {
			m.cwd, m.cursor, m.offset = home, 0, 0
			m.load()
		}
	case " ":
		if e := m.current(); e != nil {
			if m.selected[e.path] {
				delete(m.selected, e.path)
			} else {
				m.selected[e.path] = true
			}
			if m.cursor < len(m.entries)-1 {
				m.cursor++
			}
		}
	case "a":
		all := true
		for _, e := range m.entries {
			if !m.selected[e.path] {
				all = false
			}
		}
		for _, e := range m.entries {
			if all {
				delete(m.selected, e.path)
			} else {
				m.selected[e.path] = true
			}
		}
	case "esc":
		m.selected = map[string]bool{}
	case ".":
		m.cfg.ShowHidden = !m.cfg.ShowHidden
		m.load()
	case "/":
		m.typing = true
	case "r":
		m.load()
	case "c":
		if t := m.picked(); len(t) > 0 {
			m.openCompress(t, "")
		}
	case "e":
		if t := m.picked(); len(t) > 0 {
			m.openCompress(t, "age")
		}
	case "x":
		var arch []string
		for _, p := range m.picked() {
			if engine.IsArchive(p) {
				arch = append(arch, p)
			}
		}
		if len(arch) == 0 {
			m.status = "nothing to extract: select one or more archives"
			return nil
		}
		m.openExtract(arch)
	case "v":
		if e := m.current(); e != nil && e.archive != "" {
			return m.openContents(e.path, "")
		}
	case "t":
		var arch []string
		for _, p := range m.picked() {
			if engine.IsArchive(p) {
				arch = append(arch, p)
			}
		}
		for _, a := range arch {
			m.enqueue(&job{kind: jobTest, inputs: []string{a}, eopts: m.extractDefaults()})
		}
		if len(arch) > 0 {
			m.screen = scQueue
			return m.startNext()
		}
	case "tab":
		m.screen = scQueue
	case "?":
		m.back, m.screen = scBrowse, scHelp
	}
	return nil
}

func (m *model) viewBrowse() string {
	var b strings.Builder
	head := titleStyle.Render("pakket") + "  " + pathStyle.Render(tildify(m.cwd))
	if n := len(m.selected); n > 0 {
		head += "  " + markStyle.Render(fmt.Sprintf("%d selected", n))
	}
	if q := m.activeJobs(); q > 0 {
		head += "  " + keyStyle.Render(fmt.Sprintf("⟳ %d job(s)", q))
	}
	b.WriteString(head + "\n\n")
	h := m.listHeight()
	if m.cursor < m.offset {
		m.offset = m.cursor
	}
	if m.cursor >= m.offset+h {
		m.offset = m.cursor - h + 1
	}
	if len(m.entries) == 0 {
		b.WriteString(mutedStyle.Render("  (empty)") + "\n")
	}
	w := max(40, m.width)
	for i := m.offset; i < len(m.entries) && i < m.offset+h; i++ {
		e := m.entries[i]
		mark := "  "
		if m.selected[e.path] {
			mark = markStyle.Render("● ")
		}
		name, info := e.name, ""
		switch {
		case e.dir:
			name = dirStyle.Render(e.name + "/")
		case e.archive != "":
			name = archStyle.Render(e.name)
			info = archStyle.Render(e.archive) + "  " + human(e.size)
		default:
			info = human(e.size)
		}
		pad := w - 4 - lipgloss.Width(name) - lipgloss.Width(info)
		if pad < 1 {
			pad = 1
		}
		line := mark + name + strings.Repeat(" ", pad) + mutedStyle.Render(info)
		if i == m.cursor {
			line = cursorStyle.Render(lipgloss.NewStyle().Width(w - 1).Render(line))
		}
		b.WriteString(line + "\n")
	}
	for i := len(m.entries) - m.offset; i < h; i++ {
		b.WriteString("\n")
	}
	foot := keys("space", "select", "c", "compress", "x", "extract", "e", "encrypt", "v", "contents", "t", "test", "tab", "queue", "/", "filter", "?", "help", "q", "quit")
	if m.typing {
		foot = keyStyle.Render("filter: ") + m.filter + "▏  " + mutedStyle.Render("enter keep · esc clear")
	} else if m.status != "" {
		foot = badStyle.Render(m.status)
	} else if m.filter != "" {
		foot = mutedStyle.Render("filter: "+m.filter+" (/ to change)  ") + foot
	}
	b.WriteString("\n" + foot)
	return b.String()
}

func keys(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		parts = append(parts, keyStyle.Render(kv[i])+" "+mutedStyle.Render(kv[i+1]))
	}
	return strings.Join(parts, "  ")
}

func tildify(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}

func human(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

var levelNames = []string{"store", "fastest", "fast", "fast", "normal", "normal", "good", "good", "max", "ultra"}

func (m *model) openCompress(targets []string, forced string) {
	c := m.cfg
	formats := engine.CreateNames()
	fidx := 0
	want := c.Format
	if forced != "" {
		want = forced
	}
	for i, n := range formats {
		if n == want {
			fidx = i
		}
	}
	splitLabels := make([]string, 0, len(engine.SplitPresets)+1)
	sidx := 0
	for i, p := range engine.SplitPresets {
		splitLabels = append(splitLabels, p.Label)
		if p.Size == c.Split {
			sidx = i
		}
	}
	splitLabels = append(splitLabels, "custom")
	customSplit := ""
	if sidx == 0 && c.Split != "" && c.Split != "0" {
		sidx, customSplit = len(splitLabels)-1, c.Split
	}
	is := func(names ...string) func(*form) bool {
		return func(fm *form) bool {
			v := fm.get("format").value()
			for _, n := range names {
				if v == n {
					return true
				}
			}
			return false
		}
	}
	hasPw := func(fm *form) bool { return fm.get("password").value() != "" }
	canPw := is("7z", "zip", "dmg", "age")
	checksums := []string{"none", "md5", "sha256"}
	cidx := 0
	for i, s := range checksums {
		if s == c.Checksum {
			cidx = i
		}
	}
	name := ""
	if len(targets) == 1 {
		name = filepath.Base(targets[0])
	} else {
		name = "Archive"
	}
	sub := fmt.Sprintf("%d item(s): %s", len(targets), summarize(targets))
	m.form = &form{
		title: "Compress", sub: sub,
		fields: []*field{
			{key: "format", label: "Format", kind: fChoice, choices: formats, idx: fidx, hint: "7z and zip can be encrypted"},
			{key: "level", label: "Compression", kind: fSlider, val: c.Level, min: 0, max: 9, names: levelNames, visible: func(fm *form) bool { return !is("tar", "wim")(fm) }},
			{key: "password", label: "Password", kind: fText, input: newText("none", "", true), visible: canPw},
			{key: "repeat", label: "Repeat password", kind: fText, input: newText("", "", true), visible: func(fm *form) bool { return canPw(fm) && hasPw(fm) }},
			{key: "names", label: "Encrypt file names", kind: fToggle, on: c.EncryptNames, visible: func(fm *form) bool { return is("7z")(fm) && hasPw(fm) }},
			{key: "zipenc", label: "Zip encryption", kind: fChoice, choices: []string{"AES-256", "ZipCrypto (legacy)"}, idx: b2i(c.ZipLegacy), visible: func(fm *form) bool { return is("zip")(fm) && hasPw(fm) }, hint: "legacy opens in Archive Utility but is weak"},
			{key: "keychain", label: "Save in Keychain", kind: fToggle, visible: func(fm *form) bool { return canPw(fm) && hasPw(fm) }},
			{key: "split", label: "Split into volumes", kind: fChoice, choices: splitLabels, idx: sidx},
			{key: "splitsize", label: "Volume size", kind: fText, input: newText("e.g. 250m, 2g", customSplit, false), visible: func(fm *form) bool { return fm.get("split").value() == "custom" }},
			{key: "solid", label: "Solid archive", kind: fToggle, on: c.Solid, visible: is("7z")},
			{key: "mac", label: "Exclude Mac files", kind: fToggle, on: c.ExcludeMac, hint: ".DS_Store, ._ files, __MACOSX"},
			{key: "separate", label: "Archive each separately", kind: fToggle, on: c.Separate, visible: func(*form) bool { return len(targets) > 1 }},
			{key: "delete", label: "Delete originals after", kind: fToggle, on: c.DeleteAfterPack, hint: "moved to the Trash"},
			{key: "name", label: "Archive name", kind: fText, input: newText("", name, false), visible: func(fm *form) bool { return len(targets) == 1 || !fm.get("separate").on }},
			{key: "volume", label: "Volume name", kind: fText, input: newText("same as archive", "", false), visible: is("dmg", "iso")},
			{key: "dest", label: "Save to", kind: fText, input: newText("next to the originals", c.CompressDest, false)},
			{key: "checksum", label: "Checksum file", kind: fChoice, choices: checksums, idx: cidx},
			{key: "notify", label: "Notify when done", kind: fToggle, on: c.Notify},
		},
		buttons: []string{"Compress", "Save as default", "Cancel"},
	}
	m.targets = targets
	m.form.setFocus(0)
	m.screen = scCompress
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func summarize(paths []string) string {
	var names []string
	for i, p := range paths {
		if i == 3 {
			names = append(names, fmt.Sprintf("+%d more", len(paths)-3))
			break
		}
		names = append(names, filepath.Base(p))
	}
	return strings.Join(names, ", ")
}

var encodings = []string{"auto", "cp437", "shift_jis", "gbk", "big5", "euc-kr", "cp866", "windows-1251", "windows-1252", "cp850"}

func (m *model) openExtract(targets []string) {
	c := m.cfg
	pick := func(list []string, v string) int {
		for i, s := range list {
			if s == v {
				return i
			}
		}
		return 0
	}
	folders := []string{"smart", "always", "never"}
	overwrite := []string{"rename", "overwrite", "skip"}
	m.form = &form{
		title: "Extract", sub: fmt.Sprintf("%d archive(s): %s", len(targets), summarize(targets)),
		fields: []*field{
			{key: "dest", label: "Extract to", kind: fText, input: newText("next to the archive", c.ExtractDest, false)},
			{key: "folder", label: "Create a folder", kind: fChoice, choices: folders, idx: pick(folders, c.Folder), hint: "smart: only when there is more than one item"},
			{key: "password", label: "Password", kind: fText, input: newText("ask when needed", "", true)},
			{key: "overwrite", label: "Existing items", kind: fChoice, choices: overwrite, idx: pick(overwrite, c.Overwrite)},
			{key: "encoding", label: "Name encoding", kind: fChoice, choices: encodings, idx: pick(encodings, c.Encoding), hint: "for old zips with garbled names"},
			{key: "nested", label: "Unpack inner tar", kind: fToggle, on: c.Nested},
			{key: "quarantine", label: "Keep quarantine flag", kind: fToggle, on: c.Quarantine, hint: "Gatekeeper still checks downloaded apps"},
			{key: "delete", label: "Delete archive after", kind: fToggle, on: c.DeleteAfterX, hint: "moved to the Trash"},
			{key: "notify", label: "Notify when done", kind: fToggle, on: c.Notify},
		},
		buttons: []string{"Extract", "Save as default", "Cancel"},
	}
	m.targets = targets
	m.form.setFocus(0)
	m.screen = scExtract
}

func expand(p string) string {
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			p = home + p[1:]
		}
	}
	return p
}

func (m *model) updateForm(k tea.KeyMsg) tea.Cmd {
	if k.String() == "esc" {
		m.screen = scBrowse
		return nil
	}
	btn, cmd := m.form.update(k)
	if btn < 0 {
		return cmd
	}
	switch m.form.buttons[btn] {
	case "Cancel":
		m.screen = scBrowse
		return nil
	case "Save as default":
		m.saveDefaults()
		return nil
	}
	fm := m.form
	fm.err = ""
	if m.screen == scCompress {
		o, err := m.compressOptions()
		if err != nil {
			fm.err = err.Error()
			return nil
		}
		keychain := fm.get("keychain").on && o.Password != ""
		m.enqueue(&job{kind: jobCompress, inputs: m.targets, copts: o, keychain: keychain, notify: fm.get("notify").on})
	} else {
		o := m.extractOptions()
		for _, a := range m.targets {
			m.enqueue(&job{kind: jobExtract, inputs: []string{a}, eopts: o, notify: fm.get("notify").on})
		}
	}
	m.selected = map[string]bool{}
	m.screen = scQueue
	return m.startNext()
}

func (m *model) compressOptions() (engine.CompressOptions, error) {
	fm := m.form
	o := engine.CompressOptions{
		Format:       fm.get("format").value(),
		Level:        fm.get("level").val,
		ExcludeMac:   fm.get("mac").on,
		Solid:        fm.get("solid").on,
		EncryptNames: fm.get("names").on,
		ZipLegacy:    fm.get("zipenc").idx == 1,
		Separate:     fm.get("separate").on && len(m.targets) > 1,
		DeleteAfter:  fm.get("delete").on,
		VolumeName:   fm.get("volume").value(),
		DestDir:      expand(fm.get("dest").value()),
	}
	if fm.get("password").visible(fm) {
		o.Password = fm.get("password").value()
		if o.Password != fm.get("repeat").value() {
			return o, errors.New("the passwords do not match")
		}
	}
	if o.Format == "age" && o.Password == "" {
		return o, errors.New("age needs a password")
	}
	if n := strings.TrimSpace(fm.get("name").value()); n != "" && !o.Separate {
		if len(m.targets) == 1 && n == filepath.Base(m.targets[0]) {
			n = ""
		}
		o.Name = n
	}
	if c := fm.get("checksum").value(); c != "none" {
		o.Checksum = c
	}
	split := fm.get("split")
	size := "0"
	if split.value() == "custom" {
		size = fm.get("splitsize").value()
	} else {
		size = engine.SplitPresets[split.idx].Size
	}
	n, err := engine.ParseSize(size)
	if err != nil {
		return o, err
	}
	o.SplitSize = n
	return o, nil
}

func (m *model) extractOptions() engine.ExtractOptions {
	fm := m.form
	return engine.ExtractOptions{
		DestDir:     expand(fm.get("dest").value()),
		Folder:      fm.get("folder").value(),
		Password:    fm.get("password").value(),
		Overwrite:   fm.get("overwrite").value(),
		Encoding:    fm.get("encoding").value(),
		Nested:      fm.get("nested").on,
		Quarantine:  fm.get("quarantine").on,
		DeleteAfter: fm.get("delete").on,
	}
}

func (m *model) extractDefaults() engine.ExtractOptions {
	c := m.cfg
	return engine.ExtractOptions{DestDir: c.ExtractDest, Folder: c.Folder, Overwrite: c.Overwrite, Encoding: c.Encoding,
		Nested: c.Nested, Quarantine: c.Quarantine, DeleteAfter: c.DeleteAfterX}
}

func (m *model) saveDefaults() {
	fm := m.form
	c := m.cfg
	if m.screen == scCompress {
		c.Format = fm.get("format").value()
		c.Level = fm.get("level").val
		c.EncryptNames = fm.get("names").on
		c.ZipLegacy = fm.get("zipenc").idx == 1
		c.Solid = fm.get("solid").on
		c.ExcludeMac = fm.get("mac").on
		c.Separate = fm.get("separate").on
		c.DeleteAfterPack = fm.get("delete").on
		c.CompressDest = fm.get("dest").value()
		c.Notify = fm.get("notify").on
		if v := fm.get("checksum").value(); v != "none" {
			c.Checksum = v
		} else {
			c.Checksum = ""
		}
		if s := fm.get("split"); s.value() == "custom" {
			c.Split = fm.get("splitsize").value()
		} else {
			c.Split = engine.SplitPresets[s.idx].Size
			if c.Split == "0" {
				c.Split = ""
			}
		}
	} else {
		c.ExtractDest = fm.get("dest").value()
		c.Folder = fm.get("folder").value()
		c.Overwrite = fm.get("overwrite").value()
		c.Encoding = fm.get("encoding").value()
		c.Nested = fm.get("nested").on
		c.Quarantine = fm.get("quarantine").on
		c.DeleteAfterX = fm.get("delete").on
		c.Notify = fm.get("notify").on
	}
	if err := config.Save(c); err != nil {
		fm.err = err.Error()
		return
	}
	m.cfg = c
	fm.err = ""
	fm.sub = "Saved as default in " + tildify(config.Path())
}

func (m *model) View() string {
	if m.width == 0 {
		return ""
	}
	var s string
	switch m.screen {
	case scBrowse:
		s = m.viewBrowse()
	case scCompress, scExtract:
		s = m.form.view(m.width)
	case scQueue:
		s = m.viewQueue()
	case scContents:
		s = m.viewContents()
	case scPassword:
		s = m.pwForm.view(m.width)
	case scHelp:
		s = helpText
	}
	return lipgloss.NewStyle().Padding(0, 1).MaxWidth(m.width).MaxHeight(m.height).Render(s)
}

var helpText = titleStyle.Render("pakket keys") + `

` + keyStyle.Render("Browse") + `
  ↑↓ / j k        move              enter / → / l   open folder, view archive
  ← / h / ⌫       parent folder     ~               home folder
  space           select            a               select all / none
  esc             clear selection   .               show hidden files
  /               filter by name    r               refresh

` + keyStyle.Render("Actions") + ` (on the selection, or the item under the cursor)
  c   compress           x   extract archives      e   encrypt with age
  v   list contents      t   test integrity        tab job queue

` + keyStyle.Render("Queue") + `
  ↑↓ move · d / del cancel or remove a job · p enter a password · o open in Finder · esc back

` + keyStyle.Render("Scripts") + `
  pakket compress -f zip -p secret folder/   pakket extract -d out archive.rar
  pakket list --json archive.7z              pakket formats

` + mutedStyle.Render("press any key")

// startNext runs the next waiting job if none is running.
func (m *model) startNext() tea.Cmd {
	if m.running {
		return nil
	}
	for _, j := range m.jobs {
		if j.status == stWaiting {
			m.running = true
			j.status = stRunning
			j.started = time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			j.cancel = cancel
			go m.runJob(ctx, j)
			return nil
		}
	}
	return nil
}

func (m *model) activeJobs() int {
	n := 0
	for _, j := range m.jobs {
		if j.status == stWaiting || j.status == stRunning {
			n++
		}
	}
	return n
}

func itoa(i int) string { return strconv.Itoa(i) }
