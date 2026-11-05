package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Tool finds an external program. 7-Zip is looked up as 7zz, then 7z.
func Tool(name string) (string, bool) {
	names := []string{name}
	if name == "7zz" {
		names = []string{"7zz", "7z", "7za"}
	}
	for _, n := range names {
		if p, err := exec.LookPath(n); err == nil {
			return p, true
		}
	}
	return "", false
}

func need(name string) (string, error) {
	if p, ok := Tool(name); ok {
		return p, nil
	}
	hint := map[string]string{
		"7zz":     "install 7-Zip: brew install sevenzip",
		"unar":    "install The Unarchiver CLI: brew install unar",
		"lrzip":   "install lrzip: brew install lrzip",
		"hdiutil": "disk images need macOS",
		"aa":      "Apple Archive needs macOS",
		"xip":     "xip archives need macOS",
	}[name]
	if hint == "" {
		hint = "install " + name
	}
	return "", fmt.Errorf("%s not found (%s)", name, hint)
}

var pctRe = regexp.MustCompile(`(\d{1,3})%`)

// run7z runs 7-Zip. The password, if any, goes through stdin, never argv.
// pct receives 0..100 as 7-Zip reports it.
func run7z(ctx context.Context, dir string, args []string, password string, pct func(int)) error {
	bin, err := need("7zz")
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	// Creating: "-p" makes 7-Zip read the password from stdin. Reading: 7-Zip
	// asks on stdin only when the archive is encrypted (a bare "-p" would mean
	// an empty password), so an empty line means "no password".
	cmd.Stdin = strings.NewReader(password + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var stdout bytes.Buffer
	sc := bufio.NewScanner(out)
	sc.Split(scanProgress)
	for sc.Scan() {
		t := sc.Text()
		stdout.WriteString(t + "\n")
		if m := pctRe.FindStringSubmatch(t); m != nil && pct != nil {
			if v, err := strconv.Atoi(m[1]); err == nil {
				pct(v)
			}
		}
	}
	err = cmd.Wait()
	msg := stderr.String() + stdout.String()
	if strings.Contains(msg, "Wrong password") || strings.Contains(msg, "Cannot open encrypted archive") {
		return ErrPassword
	}
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("7-Zip: %s", lastLines(msg, 4))
	}
	return nil
}

// scanProgress splits on \n, \r and backspaces so 7-Zip's live counter is seen.
func scanProgress(data []byte, atEOF bool) (int, []byte, error) {
	for i, b := range data {
		if b == '\n' || b == '\r' || b == '\b' {
			return i + 1, bytes.TrimSpace(data[:i]), nil
		}
	}
	if atEOF && len(data) > 0 {
		return len(data), data, nil
	}
	return 0, nil, nil
}

func lastLines(s string, n int) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !pctRe.MatchString(l) {
			keep = append(keep, l)
		}
	}
	if len(keep) > n {
		keep = keep[len(keep)-n:]
	}
	return strings.Join(keep, "; ")
}

// runTool runs a program, feeding stdin, and returns combined output on failure.
func runTool(ctx context.Context, dir string, stdin string, name string, args ...string) error {
	bin, err := need(name)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s := string(out)
		if strings.Contains(s, "Authentication error") || strings.Contains(s, "authenticate") {
			return ErrPassword
		}
		return fmt.Errorf("%s: %s", name, lastLines(s, 3))
	}
	return nil
}

// Trash moves paths to the Trash: macOS, or gio / trash-cli on Linux. With
// neither available it deletes them.
func Trash(paths ...string) error {
	if len(paths) == 0 {
		return nil
	}
	if runtime.GOOS == "darwin" {
		if bin, ok := Tool("trash"); ok {
			if out, err := exec.Command(bin, paths...).CombinedOutput(); err != nil {
				return fmt.Errorf("trash: %s", strings.TrimSpace(string(out)))
			}
			return nil
		}
		home, _ := os.UserHomeDir()
		for _, p := range paths {
			dst := uniquePath(filepath.Join(home, ".Trash", filepath.Base(p)), false)
			if err := os.Rename(p, dst); err != nil {
				return err
			}
		}
		return nil
	}
	// Linux desktops: the freedesktop Trash through gio or trash-cli.
	if bin, ok := Tool("gio"); ok {
		if err := exec.Command(bin, append([]string{"trash", "--"}, paths...)...).Run(); err == nil {
			return nil
		}
	}
	if bin, ok := Tool("trash-put"); ok {
		if err := exec.Command(bin, append([]string{"--"}, paths...)...).Run(); err == nil {
			return nil
		}
	}
	for _, p := range paths {
		if err := os.RemoveAll(p); err != nil {
			return err
		}
	}
	return nil
}

const quarantineAttr = "com.apple.quarantine"

func getQuarantine(p string) []byte {
	if runtime.GOOS != "darwin" {
		return nil
	}
	buf := make([]byte, 512)
	n, err := unix.Getxattr(p, quarantineAttr, buf)
	if err != nil || n <= 0 {
		return nil
	}
	return buf[:n]
}

// applyQuarantine marks every extracted item like the archive was marked,
// as Archive Utility does, so Gatekeeper still checks downloaded apps.
func applyQuarantine(root string, val []byte) {
	if len(val) == 0 {
		return
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink == 0 {
			_ = unix.Setxattr(p, quarantineAttr, val, 0)
		}
		return nil
	})
}

// Notify shows a desktop notification (macOS only, best effort).
func Notify(title, msg string) {
	if runtime.GOOS != "darwin" {
		return
	}
	q := func(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) }
	_ = exec.Command("osascript", "-e", fmt.Sprintf(`display notification "%s" with title "%s"`, q(msg), q(title))).Run()
}

// KeychainSave stores a password for an archive name in the login Keychain.
func KeychainSave(archive, password string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("the Keychain needs macOS")
	}
	// security reads the password interactively only from a tty, so it is
	// passed as an argument; it is visible to local `ps` for a moment.
	out, err := exec.Command("security", "add-generic-password", "-U", "-s", "pakket", "-a", filepath.Base(archive),
		"-l", "pakket: "+filepath.Base(archive), "-w", password).CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// KeychainFind returns a stored password for an archive, if any. It also
// tries the name without split or extension suffixes.
func KeychainFind(archive string) (string, bool) {
	if runtime.GOOS != "darwin" {
		return "", false
	}
	base := filepath.Base(archive)
	for _, n := range []string{base, stripSplit(base)} {
		out, err := exec.Command("security", "find-generic-password", "-s", "pakket", "-a", n, "-w").Output()
		if err == nil {
			return strings.TrimRight(string(out), "\n"), true
		}
	}
	return "", false
}

// KeychainDelete removes a stored password.
func KeychainDelete(archive string) error {
	out, err := exec.Command("security", "delete-generic-password", "-s", "pakket", "-a", filepath.Base(archive)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("keychain: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// writeChecksum writes "<archive>.md5" or ".sha256" in the usual format.
func writeChecksum(kind string, files []string) error {
	if kind == "" {
		return nil
	}
	for _, f := range files {
		var h hash.Hash
		switch kind {
		case "md5":
			h = md5.New()
		case "sha256":
			h = sha256.New()
		default:
			return fmt.Errorf("unknown checksum %q (md5, sha256)", kind)
		}
		in, err := os.Open(f)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, in)
		in.Close()
		if err != nil {
			return err
		}
		line := fmt.Sprintf("%s  %s\n", hex.EncodeToString(h.Sum(nil)), filepath.Base(f))
		if err := os.WriteFile(f+"."+kind, []byte(line), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// splitWriter writes base.001, base.002 ... of at most size bytes each.
type splitWriter struct {
	base  string
	size  int64
	n     int
	cur   *os.File
	left  int64
	parts []string
}

func newSplitWriter(base string, size int64) *splitWriter {
	return &splitWriter{base: base, size: size}
}

func (s *splitWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		if s.cur == nil || s.left == 0 {
			if s.cur != nil {
				if err := s.cur.Close(); err != nil {
					return written, err
				}
			}
			s.n++
			name := fmt.Sprintf("%s.%03d", s.base, s.n)
			f, err := os.Create(name)
			if err != nil {
				return written, err
			}
			s.cur, s.left = f, s.size
			s.parts = append(s.parts, name)
		}
		chunk := p
		if int64(len(chunk)) > s.left {
			chunk = chunk[:s.left]
		}
		n, err := s.cur.Write(chunk)
		written += n
		s.left -= int64(n)
		p = p[n:]
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (s *splitWriter) Close() error {
	if s.cur != nil {
		return s.cur.Close()
	}
	return nil
}

// splitFile cuts an existing file into volumes and removes the original.
func splitFile(path string, size int64) ([]string, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	sw := newSplitWriter(path, size)
	_, err = io.Copy(sw, in)
	in.Close()
	if cerr := sw.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		for _, p := range sw.parts {
			os.Remove(p)
		}
		return nil, err
	}
	return sw.parts, os.Remove(path)
}

// Parts returns every volume of a split archive given any one of them,
// or just the path itself.
func Parts(path string) []string {
	base, ok := splitBase(path)
	if !ok {
		return []string{path}
	}
	matches, _ := filepath.Glob(globEscape(base) + ".[0-9][0-9][0-9]")
	sort.Strings(matches)
	if len(matches) == 0 {
		return []string{path}
	}
	return matches
}

func globEscape(s string) string {
	r := strings.NewReplacer(`*`, `\*`, `?`, `\?`, `[`, `\[`, `\`, `\\`)
	return r.Replace(s)
}

// joinedFile is a read-only view over split volumes.
type joinedFile struct {
	io.Reader
	files []*os.File
	size  int64
}

func openJoined(path string) (*joinedFile, error) {
	parts := Parts(path)
	j := &joinedFile{}
	var rs []io.Reader
	for _, p := range parts {
		f, err := os.Open(p)
		if err != nil {
			j.Close()
			return nil, err
		}
		st, _ := f.Stat()
		j.size += st.Size()
		j.files = append(j.files, f)
		rs = append(rs, f)
	}
	j.Reader = io.MultiReader(rs...)
	return j, nil
}

func (j *joinedFile) Close() error {
	for _, f := range j.files {
		f.Close()
	}
	return nil
}

// joinToTemp concatenates split volumes into one temp file for tools that
// need a single seekable file (zip, dmg).
func joinToTemp(path, dir string) (string, func(), error) {
	parts := Parts(path)
	if len(parts) == 1 {
		return parts[0], func() {}, nil
	}
	j, err := openJoined(path)
	if err != nil {
		return "", nil, err
	}
	defer j.Close()
	tmp, err := os.CreateTemp(dir, ".pakket-join-*"+filepath.Ext(stripSplit(path)))
	if err != nil {
		return "", nil, err
	}
	if _, err := io.Copy(tmp, j); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return "", nil, err
	}
	tmp.Close()
	return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
}
