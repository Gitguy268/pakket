package tui

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Gitguy268/pakket/internal/engine"
)

type jobKind int

const (
	jobCompress jobKind = iota
	jobExtract
	jobTest
)

type jobStatus int

const (
	stWaiting jobStatus = iota
	stRunning
	stDone
	stFailed
	stCancelled
	stNeedPassword
)

type job struct {
	kind     jobKind
	inputs   []string
	copts    engine.CompressOptions
	eopts    engine.ExtractOptions
	keychain bool
	notify   bool

	status  jobStatus
	done    int64
	total   int64
	current string
	result  []string
	err     error
	started time.Time
	elapsed time.Duration
	cancel  context.CancelFunc
}

type progressMsg struct {
	j           *job
	done, total int64
	current     string
}

type jobDoneMsg struct {
	j      *job
	result []string
	err    error
}

type contentsMsg struct {
	archive string
	entries []engine.Entry
	err     error
}

func (m *model) enqueue(j *job) { m.jobs = append(m.jobs, j) }

// runJob runs in its own goroutine and reports through m.events.
func (m *model) runJob(ctx context.Context, j *job) {
	var mu sync.Mutex
	last := time.Time{}
	cb := func(done, total int64, cur string) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(last) < 60*time.Millisecond {
			return
		}
		last = time.Now()
		select {
		case m.events <- progressMsg{j, done, total, cur}:
		default:
		}
	}
	var res []string
	var err error
	switch j.kind {
	case jobCompress:
		res, err = engine.Compress(ctx, j.inputs, j.copts, cb)
		if err == nil && j.keychain {
			for _, r := range res {
				if kerr := engine.KeychainSave(r, j.copts.Password); kerr != nil {
					err = kerr
				}
			}
		}
	case jobExtract:
		o := j.eopts
		if m.cfg.Keychain {
			if pw, ok := engine.KeychainFind(j.inputs[0]); ok {
				o.Passwords = append(o.Passwords, pw)
			}
		}
		var r string
		r, err = engine.Extract(ctx, j.inputs[0], o, cb)
		if r != "" {
			res = []string{r}
		}
	case jobTest:
		err = engine.Test(ctx, j.inputs[0], j.eopts, cb)
	}
	m.events <- jobDoneMsg{j, res, err}
}

func (m *model) handleEvent(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case progressMsg:
		if msg.j.status == stRunning {
			msg.j.done, msg.j.total, msg.j.current = msg.done, msg.total, msg.current
		}
	case jobDoneMsg:
		j := msg.j
		m.running = false
		j.elapsed = time.Since(j.started)
		j.result, j.err = msg.result, msg.err
		switch {
		case msg.err == nil:
			j.status = stDone
		case errors.Is(msg.err, context.Canceled):
			j.status = stCancelled
		case errors.Is(msg.err, engine.ErrPassword):
			j.status = stNeedPassword
			if m.screen != scPassword && m.screen != scCompress && m.screen != scExtract {
				m.askPassword(j, "")
			}
		default:
			j.status = stFailed
		}
		if j.notify && j.status != stNeedPassword {
			go engine.Notify("pakket", jobLine(j))
		}
		m.load()
		return m.startNext()
	case contentsMsg:
		if msg.archive != m.contentsOf {
			return nil
		}
		if errors.Is(msg.err, engine.ErrPassword) {
			m.pwList = msg.archive
			m.askPassword(nil, msg.archive)
			return nil
		}
		m.contents, m.contentsErr = msg.entries, ""
		if msg.err != nil {
			m.contentsErr = msg.err.Error()
		}
	}
	return nil
}

func jobLine(j *job) string {
	name := filepath.Base(j.inputs[0])
	if len(j.inputs) > 1 {
		name = fmt.Sprintf("%s +%d", name, len(j.inputs)-1)
	}
	verb := map[jobKind]string{jobCompress: "Compress", jobExtract: "Extract", jobTest: "Test"}[j.kind]
	switch j.status {
	case stDone:
		if j.kind == jobTest {
			return name + " is OK"
		}
		if len(j.result) > 0 {
			return fmt.Sprintf("%s: %s → %s", verb, name, filepath.Base(j.result[0]))
		}
		return verb + ": " + name + " done"
	case stFailed:
		return fmt.Sprintf("%s failed: %s: %v", verb, name, j.err)
	}
	return verb + ": " + name
}

func (m *model) askPassword(j *job, listArchive string) {
	target := listArchive
	if j != nil {
		target = j.inputs[0]
	}
	m.pwJob = j
	m.pwForm = &form{
		title: "Password needed",
		sub:   filepath.Base(target) + " is encrypted, or the password was wrong.",
		fields: []*field{
			{key: "password", label: "Password", kind: fText, input: newText("", "", true)},
			{key: "keychain", label: "Save in Keychain", kind: fToggle},
		},
		buttons: []string{"OK", "Skip"},
	}
	m.pwForm.setFocus(0)
	if m.screen != scPassword {
		m.back = m.screen
	}
	m.screen = scPassword
}

func (m *model) updatePassword(k tea.KeyMsg) tea.Cmd {
	if k.String() == "esc" {
		m.screen = m.back
		return nil
	}
	if k.String() == "enter" {
		if f, _ := m.pwForm.current(); f != nil && f.key == "password" {
			m.pwForm.setFocus(len(m.pwForm.visibleFields())) // jump to OK
			return m.submitPassword()
		}
	}
	btn, cmd := m.pwForm.update(k)
	switch btn {
	case 0:
		return m.submitPassword()
	case 1:
		m.screen = m.back
	}
	return cmd
}

func (m *model) submitPassword() tea.Cmd {
	pw := m.pwForm.get("password").value()
	if pw == "" {
		m.pwForm.err = "type the password, or choose Skip"
		return nil
	}
	save := m.pwForm.get("keychain").on
	m.screen = m.back
	if m.pwJob == nil { // listing contents
		a := m.pwList
		if save {
			_ = engine.KeychainSave(a, pw)
		}
		return m.openContents(a, pw)
	}
	j := m.pwJob
	if save {
		_ = engine.KeychainSave(j.inputs[0], pw)
	}
	j.eopts.Password = pw
	j.status, j.err, j.done, j.total = stWaiting, nil, 0, 0
	return m.startNext()
}

func (m *model) updateQueue(k tea.KeyMsg) tea.Cmd {
	switch k.String() {
	case "esc", "tab", "q":
		m.screen = scBrowse
	case "up", "k":
		if m.qcursor > 0 {
			m.qcursor--
		}
	case "down", "j":
		if m.qcursor < len(m.jobs)-1 {
			m.qcursor++
		}
	case "d", "delete", "backspace":
		if m.qcursor < len(m.jobs) {
			j := m.jobs[m.qcursor]
			switch j.status {
			case stRunning:
				j.cancel()
			default:
				m.jobs = append(m.jobs[:m.qcursor], m.jobs[m.qcursor+1:]...)
				if m.qcursor >= len(m.jobs) && m.qcursor > 0 {
					m.qcursor--
				}
			}
		}
	case "p", "enter":
		if m.qcursor < len(m.jobs) && m.jobs[m.qcursor].status == stNeedPassword {
			m.askPassword(m.jobs[m.qcursor], "")
		}
	case "o":
		if m.qcursor < len(m.jobs) {
			if j := m.jobs[m.qcursor]; len(j.result) > 0 && runtime.GOOS == "darwin" {
				_ = exec.Command("open", "-R", j.result[0]).Start()
			}
		}
	case "C":
		var keep []*job
		for _, j := range m.jobs {
			if j.status == stWaiting || j.status == stRunning || j.status == stNeedPassword {
				keep = append(keep, j)
			}
		}
		m.jobs, m.qcursor = keep, 0
	case "?":
		m.back, m.screen = scQueue, scHelp
	}
	return nil
}

func (m *model) viewQueue() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render("Queue") + "  " + mutedStyle.Render(fmt.Sprintf("%d job(s)", len(m.jobs))) + "\n\n")
	if len(m.jobs) == 0 {
		b.WriteString(mutedStyle.Render("  Nothing queued. Select files and press c or x.") + "\n")
	}
	w := max(40, m.width-4)
	for i, j := range m.jobs {
		icon := map[jobStatus]string{stWaiting: mutedStyle.Render("…"), stRunning: keyStyle.Render("⟳"), stDone: goodStyle.Render("✓"),
			stFailed: badStyle.Render("✗"), stCancelled: mutedStyle.Render("–"), stNeedPassword: keyStyle.Render("🔒")}[j.status]
		line := icon + " " + jobLine(j)
		var detail string
		switch j.status {
		case stRunning:
			detail = progressBar(j, w)
		case stDone:
			detail = mutedStyle.Render(fmt.Sprintf("  %s", j.elapsed.Round(time.Millisecond)))
			if len(j.result) > 0 && j.kind != jobTest {
				detail += mutedStyle.Render("  " + tildify(j.result[0]))
				if len(j.result) > 1 {
					detail += mutedStyle.Render(fmt.Sprintf(" (+%d volumes)", len(j.result)-1))
				}
			}
		case stNeedPassword:
			detail = keyStyle.Render("  press p to enter the password")
		case stFailed:
			line = icon + " " + badStyle.Render(jobLine(j))
		}
		if i == m.qcursor {
			line = cursorStyle.Render(lipgloss.NewStyle().Width(w).Render(line))
		}
		b.WriteString(line + "\n")
		if detail != "" {
			b.WriteString(detail + "\n")
		}
	}
	b.WriteString("\n" + keys("↑↓", "move", "d", "cancel/remove", "p", "password", "o", "show in Finder", "C", "clear finished", "esc", "back"))
	return b.String()
}

func progressBar(j *job, w int) string {
	width := min(40, w-30)
	if width < 10 {
		width = 10
	}
	if j.total <= 0 {
		return "  " + mutedStyle.Render(human(j.done)+"  "+j.current)
	}
	pct := float64(j.done) / float64(j.total)
	if pct > 1 {
		pct = 1
	}
	n := int(pct * float64(width))
	bar := keyStyle.Render(strings.Repeat("█", n)) + mutedStyle.Render(strings.Repeat("░", width-n))
	cur := j.current
	if r := []rune(cur); len(r) > 40 {
		cur = "…" + string(r[len(r)-39:])
	}
	return fmt.Sprintf("  %s %3.0f%%  %s", bar, pct*100, mutedStyle.Render(cur))
}

func (m *model) openContents(archive, pw string) tea.Cmd {
	m.screen, m.contentsOf, m.contents, m.contentsErr, m.coffset = scContents, archive, nil, "", 0
	o := m.extractDefaults()
	o.Password = pw
	if m.cfg.Keychain {
		if kp, ok := engine.KeychainFind(archive); ok {
			o.Passwords = append(o.Passwords, kp)
		}
	}
	ch := m.events
	go func() {
		es, err := engine.List(context.Background(), archive, o)
		ch <- contentsMsg{archive, es, err}
	}()
	return nil
}

func (m *model) updateContents(k tea.KeyMsg) tea.Cmd {
	h := m.listHeight() - 2
	switch k.String() {
	case "esc", "q", "left", "h", "backspace":
		m.screen = scBrowse
	case "up", "k":
		if m.coffset > 0 {
			m.coffset--
		}
	case "down", "j":
		if m.coffset < len(m.contents)-h {
			m.coffset++
		}
	case "pgdown", " ":
		m.coffset = max(0, min(len(m.contents)-h, m.coffset+h))
	case "pgup":
		m.coffset = max(0, m.coffset-h)
	case "x":
		m.openExtract([]string{m.contentsOf})
	case "t":
		m.enqueue(&job{kind: jobTest, inputs: []string{m.contentsOf}, eopts: m.extractDefaults()})
		m.screen = scQueue
		return m.startNext()
	}
	return nil
}

func (m *model) viewContents() string {
	var b strings.Builder
	b.WriteString(titleStyle.Render(filepath.Base(m.contentsOf)) + "  " + pathStyle.Render(tildify(filepath.Dir(m.contentsOf))) + "\n\n")
	switch {
	case m.contentsErr != "":
		b.WriteString(badStyle.Render(m.contentsErr) + "\n")
	case m.contents == nil:
		b.WriteString(mutedStyle.Render("reading…") + "\n")
	default:
		h := m.listHeight() - 2
		var total int64
		files := 0
		for _, e := range m.contents {
			if !e.Dir {
				files++
				if e.Size > 0 {
					total += e.Size
				}
			}
		}
		b.WriteString(mutedStyle.Render(fmt.Sprintf("%10s %10s  %-16s  %s", "size", "packed", "modified", "name")) + "\n")
		for i := m.coffset; i < len(m.contents) && i < m.coffset+h; i++ {
			e := m.contents[i]
			size, packed, mod := "", "", ""
			if e.Dir {
				size = "dir"
			} else if e.Size >= 0 {
				size = human(e.Size)
			}
			if e.Packed >= 0 && !e.Dir {
				packed = human(e.Packed)
			}
			if !e.Modified.IsZero() {
				mod = e.Modified.Local().Format("2006-01-02 15:04")
			}
			name := e.Name
			if e.Dir {
				name = dirStyle.Render(name + "/")
			}
			if e.Encrypted {
				name += keyStyle.Render(" 🔒")
			}
			b.WriteString(fmt.Sprintf("%10s %10s  %-16s  %s\n", size, packed, mod, name))
		}
		b.WriteString("\n" + mutedStyle.Render(fmt.Sprintf("%d files, %s", files, human(total))) + "\n")
	}
	b.WriteString("\n" + keys("↑↓", "scroll", "x", "extract", "t", "test", "esc", "back"))
	return b.String()
}
