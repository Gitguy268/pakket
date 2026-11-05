package main

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// bar draws a one-line progress bar on stderr when it is a terminal.
type bar struct {
	mu    sync.Mutex
	on    bool
	last  time.Time
	drawn bool
}

func newBar(quiet bool) *bar {
	return &bar{on: !quiet && term.IsTerminal(int(os.Stderr.Fd()))}
}

func (b *bar) update(done, total int64, name string) {
	if !b.on {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.last) < 80*time.Millisecond && (total == 0 || done < total) {
		return
	}
	b.last = time.Now()
	width := 80
	if w, _, err := term.GetSize(int(os.Stderr.Fd())); err == nil && w > 20 {
		width = w
	}
	var line string
	if total > 0 {
		pct := float64(done) / float64(total)
		if pct > 1 {
			pct = 1
		}
		n := int(pct * 24)
		line = fmt.Sprintf("[%s%s] %3.0f%% %s/%s ", strings.Repeat("█", n), strings.Repeat("░", 24-n), pct*100, human(done), human(total))
	} else {
		line = fmt.Sprintf("%s ", human(done))
	}
	room := width - len([]rune(line)) - 1
	if r := []rune(name); room > 4 && len(r) > room {
		name = "…" + string(r[len(r)-room+1:])
	} else if room <= 4 {
		name = ""
	}
	fmt.Fprintf(os.Stderr, "\r\033[K%s%s", line, name)
	b.drawn = true
}

func (b *bar) done() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.drawn {
		fmt.Fprint(os.Stderr, "\r\033[K")
	}
}
