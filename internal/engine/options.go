package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// Progress receives updates while a job runs. done and total are bytes;
// total is 0 when unknown. It may be nil.
type Progress func(done, total int64, current string)

// CompressOptions are the settings for making archives.
type CompressOptions struct {
	Format         string   // see CreateNames
	Level          int      // 0 (store) .. 9 (max)
	Password       string   // empty = no encryption
	EncryptNames   bool     // 7z only: hide file names (on by default in the CLI)
	ZipLegacy      bool     // zip: ZipCrypto instead of AES-256
	Solid          bool     // 7z solid block
	ExcludeMac     bool     // skip .DS_Store, ._* and __MACOSX
	Exclude        []string // extra glob patterns, matched on base names
	Separate       bool     // one archive per input
	DeleteAfter    bool     // move inputs to the Trash after success
	SplitSize      int64    // bytes per volume, 0 = no split
	Output         string   // full output path (single archive only)
	DestDir        string   // folder for outputs, default: next to the first input
	Name           string   // archive base name without extension
	VolumeName     string   // dmg / iso
	Checksum       string   // "", "md5", "sha256": write a sidecar file
	ExportPassword bool     // write <archive>.txt with the password
	AgeRecipients  []string // age: public keys or files of keys, used instead of a passphrase
	Overwrite      bool     // replace an existing output instead of picking "name 2"
	Threads        int      // 0 = automatic
}

// ExtractOptions are the settings for unpacking archives.
type ExtractOptions struct {
	DestDir      string   // default: next to the archive
	Folder       string   // "smart" (default), "always", "never"
	Password     string   // tried when the archive is encrypted
	Passwords    []string // more passwords to try, in order
	DeleteAfter  bool     // move the archive (and its parts) to the Trash after success
	Quarantine   bool     // copy com.apple.quarantine from the archive to its contents
	Encoding     string   // name encoding for old zips: "auto", "cp437", "shift_jis", ...
	Overwrite    string   // "rename" (default), "overwrite", "skip"
	IgnoreGzName bool     // ignore the name stored inside a .gz
	Identities   []string // age identity files
	Nested       bool     // also unpack a single archive found inside (e.g. .tar inside .gz)
}

// ErrPassword means a password is needed or the one given is wrong.
var ErrPassword = errors.New("wrong or missing password")

type unknownErr string

func (e unknownErr) Error() string { return "unknown archive format: " + string(e) }
func errUnknown(p string) error    { return unknownErr(filepath.Base(p)) }

// ParseSize reads sizes like 100m, 4.7g, 650MB, 1440k, 2048 (bytes).
func ParseSize(s string) (int64, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || s == "0" || s == "none" {
		return 0, nil
	}
	mult := int64(1)
	for _, u := range []struct {
		suf string
		m   int64
	}{{"tb", 1 << 40}, {"gb", 1 << 30}, {"mb", 1 << 20}, {"kb", 1 << 10}, {"t", 1 << 40}, {"g", 1 << 30}, {"m", 1 << 20}, {"k", 1 << 10}, {"b", 1}} {
		if strings.HasSuffix(s, u.suf) {
			mult = u.m
			s = strings.TrimSuffix(s, u.suf)
			break
		}
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("bad size %q", s)
	}
	n := int64(v * float64(mult))
	if n > 0 && n < 64<<10 {
		return 0, fmt.Errorf("split size must be at least 64k")
	}
	return n, nil
}

// SplitPresets are the sizes offered in the TUI.
var SplitPresets = []struct {
	Label string
	Size  string
}{
	{"none", "0"}, {"10 MB (email)", "10m"}, {"25 MB (email)", "25m"}, {"100 MB", "100m"},
	{"650 MB (CD)", "650m"}, {"700 MB (CD)", "700m"}, {"1 GB", "1g"},
	{"4 GB (FAT32)", "4095m"}, {"4.7 GB (DVD)", "4480m"}, {"8.5 GB (DVD DL)", "8100m"}, {"25 GB (Blu-ray)", "23g"},
}

// macJunk are the names skipped when ExcludeMac is on.
func isMacJunk(name string) bool {
	return name == ".DS_Store" || name == "__MACOSX" || strings.HasPrefix(name, "._") ||
		name == ".Spotlight-V100" || name == ".fseventsd" || name == ".Trashes" || name == ".TemporaryItems" || name == "Icon\r"
}

func excluded(name string, o *CompressOptions) bool {
	if o.ExcludeMac && isMacJunk(name) {
		return true
	}
	for _, p := range o.Exclude {
		if ok, _ := filepath.Match(p, name); ok {
			return true
		}
	}
	return false
}

// uniquePath returns p, or "p 2", "p 3"... before the extension when p exists.
func uniquePath(p string, overwrite bool) string {
	if overwrite {
		return p
	}
	if _, err := os.Lstat(p); os.IsNotExist(err) {
		return p
	}
	dir, base := filepath.Split(p)
	stem := StripExt(base)
	ext := base[len(stem):]
	if ext == base { // no known extension
		ext = filepath.Ext(base)
		stem = strings.TrimSuffix(base, ext)
	}
	for i := 2; ; i++ {
		c := filepath.Join(dir, fmt.Sprintf("%s %d%s", stem, i, ext))
		if _, err := os.Lstat(c); os.IsNotExist(err) {
			return c
		}
	}
}

// counter tracks bytes for progress reporting.
type counter struct {
	done  atomic.Int64
	total int64
	cb    Progress
	name  atomic.Value
}

func newCounter(total int64, cb Progress) *counter {
	c := &counter{total: total, cb: cb}
	c.name.Store("")
	return c
}

func (c *counter) add(n int64) {
	if c == nil {
		return
	}
	d := c.done.Add(n)
	if c.cb != nil {
		c.cb(d, c.total, c.name.Load().(string))
	}
}

func (c *counter) set(name string) {
	if c != nil {
		c.name.Store(name)
		if c.cb != nil {
			c.cb(c.done.Load(), c.total, name)
		}
	}
}

// countingReader reports bytes read, and stops when ctx is cancelled.
type countingReader struct {
	ctx context.Context
	r   interface{ Read([]byte) (int, error) }
	c   *counter
}

func (cr *countingReader) Read(p []byte) (int, error) {
	if err := cr.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := cr.r.Read(p)
	cr.c.add(int64(n))
	return n, err
}

// treeSize adds up regular file sizes under the inputs.
func treeSize(inputs []string, o *CompressOptions) int64 {
	var total int64
	for _, in := range inputs {
		_ = filepath.Walk(in, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if p != in && excluded(info.Name(), o) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if info.Mode().IsRegular() {
				total += info.Size()
			}
			return nil
		})
	}
	return total
}
