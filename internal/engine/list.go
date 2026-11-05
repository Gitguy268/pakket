package engine

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Entry is one item inside an archive.
type Entry struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`   // -1 when unknown
	Packed    int64     `json:"packed"` // -1 when unknown
	Modified  time.Time `json:"modified,omitempty"`
	Dir       bool      `json:"dir,omitempty"`
	Encrypted bool      `json:"encrypted,omitempty"`
}

// List returns the contents of an archive.
func List(ctx context.Context, archive string, o ExtractOptions) ([]Entry, error) {
	f, err := Detect(archive)
	if err != nil {
		return nil, err
	}
	var last error
	for _, pw := range passwordsFor(o) {
		es, err := listWith(ctx, f, archive, pw, o)
		if err == nil {
			return es, nil
		}
		if !errors.Is(err, ErrPassword) {
			return nil, err
		}
		last = err
	}
	return nil, last
}

func listWith(ctx context.Context, f Format, archive, pw string, o ExtractOptions) ([]Entry, error) {
	switch f.Kind {
	case KindTar:
		return listTar(ctx, f.Codec, archive)
	case KindStream:
		es, err := listTar(ctx, f.Codec, archive)
		if err == nil && len(es) > 0 {
			return es, nil
		}
		return []Entry{{Name: StripExt(filepath.Base(archive)), Size: -1, Packed: sizeOf(archive)}}, nil
	case KindZip:
		es, err := listZip(archive, o)
		if err != nil {
			return list7z(ctx, archive, pw)
		}
		return es, nil
	case KindDMG:
		if runtime.GOOS == "darwin" {
			return listByExtracting(ctx, f, archive, pw, o)
		}
		return list7z(ctx, archive, pw)
	case KindAAR:
		out, err := exec.CommandContext(ctx, "aa", "list", "-i", archive).Output()
		if err != nil {
			return nil, fmt.Errorf("aa list failed: %w", err)
		}
		var es []Entry
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if l = strings.TrimSpace(l); l != "" && l != "." {
				es = append(es, Entry{Name: l, Size: -1, Packed: -1})
			}
		}
		return es, nil
	case KindUnar, KindLrzip, KindWpress, KindAge:
		return listByExtracting(ctx, f, archive, pw, o)
	default:
		return list7z(ctx, archive, pw)
	}
}

func sizeOf(p string) int64 {
	var n int64
	for _, part := range Parts(p) {
		if st, err := os.Stat(part); err == nil {
			n += st.Size()
		}
	}
	return n
}

func listTar(ctx context.Context, codec, archive string) ([]Entry, error) {
	j, err := openJoined(archive)
	if err != nil {
		return nil, err
	}
	defer j.Close()
	dec, err := newDecoder(codec, &countingReader{ctx: ctx, r: j, c: nil})
	if err != nil {
		return nil, err
	}
	defer dec.Close()
	tr := tar.NewReader(dec)
	var es []Entry
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return es, nil
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		es = append(es, Entry{Name: strings.TrimSuffix(h.Name, "/"), Size: h.Size, Packed: -1, Modified: h.ModTime, Dir: h.Typeflag == tar.TypeDir})
	}
}

func listZip(archive string, o ExtractOptions) ([]Entry, error) {
	path, done, err := joinToTemp(archive, os.TempDir())
	if err != nil {
		return nil, err
	}
	defer done()
	zr, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	forced := o.Encoding != "" && o.Encoding != "auto"
	enc := "IBM437"
	if forced {
		enc = o.Encoding
	}
	dec, err := decoderFor(enc)
	if err != nil {
		return nil, err
	}
	var es []Entry
	for _, f := range zr.File {
		name := zipName(f, dec, forced)
		if strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		es = append(es, Entry{Name: strings.TrimSuffix(name, "/"), Size: int64(f.UncompressedSize64), Packed: int64(f.CompressedSize64),
			Modified: f.Modified, Dir: f.Mode().IsDir(), Encrypted: f.Flags&1 != 0})
	}
	return es, nil
}

func list7z(ctx context.Context, archive, pw string) ([]Entry, error) {
	bin, err := need("7zz")
	if err != nil {
		return nil, err
	}
	args := []string{"l", "-slt", "-ba"}
	args = append(args, "--", Parts(archive)[0])
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdin = strings.NewReader(pw + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	msg := stderr.String() + string(out)
	if strings.Contains(msg, "Wrong password") || strings.Contains(msg, "Cannot open encrypted archive") {
		return nil, ErrPassword
	}
	if err != nil {
		return nil, fmt.Errorf("7-Zip: %s", lastLines(msg, 3))
	}
	var es []Entry
	var cur *Entry
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " = ")
		if !ok {
			continue
		}
		switch k {
		case "Path":
			es = append(es, Entry{Name: v, Size: -1, Packed: -1})
			cur = &es[len(es)-1]
		case "Size":
			if cur != nil {
				cur.Size, _ = strconv.ParseInt(v, 10, 64)
			}
		case "Packed Size":
			if cur != nil && v != "" {
				cur.Packed, _ = strconv.ParseInt(v, 10, 64)
			}
		case "Modified":
			if cur != nil {
				cur.Modified, _ = time.ParseInLocation("2006-01-02 15:04:05", strings.SplitN(v, ".", 2)[0], time.Local)
			}
		case "Folder":
			if cur != nil {
				cur.Dir = v == "+"
			}
		case "Attributes":
			if cur != nil && strings.HasPrefix(v, "D") {
				cur.Dir = true
			}
		case "Encrypted":
			if cur != nil {
				cur.Encrypted = v == "+"
			}
		}
	}
	return es, nil
}

// listByExtracting unpacks to a temp folder and walks it.
func listByExtracting(ctx context.Context, f Format, archive, pw string, o ExtractOptions) ([]Entry, error) {
	tmp, err := os.MkdirTemp("", "pakket-list-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := extractInto(ctx, f, archive, tmp, pw, o, nil); err != nil {
		return nil, err
	}
	var es []Entry
	err = filepath.Walk(tmp, func(p string, info os.FileInfo, err error) error {
		if err != nil || p == tmp {
			return err
		}
		rel, _ := filepath.Rel(tmp, p)
		e := Entry{Name: filepath.ToSlash(rel), Size: info.Size(), Packed: -1, Modified: info.ModTime(), Dir: info.IsDir()}
		if e.Dir {
			e.Size = 0
		}
		es = append(es, e)
		return nil
	})
	return es, err
}

// Test checks that an archive is readable and its checksums match.
func Test(ctx context.Context, archive string, o ExtractOptions, cb Progress) error {
	f, err := Detect(archive)
	if err != nil {
		return err
	}
	var last error
	for _, pw := range passwordsFor(o) {
		err := testWith(ctx, f, archive, pw, o, cb)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrPassword) {
			return err
		}
		last = err
	}
	return last
}

func testWith(ctx context.Context, f Format, archive, pw string, o ExtractOptions, cb Progress) error {
	switch f.Kind {
	case KindTar, KindStream:
		j, err := openJoined(archive)
		if err != nil {
			return err
		}
		defer j.Close()
		c := newCounter(j.size, cb)
		dec, err := newDecoder(f.Codec, &countingReader{ctx: ctx, r: j, c: c})
		if err != nil {
			return err
		}
		defer dec.Close()
		br := bufio.NewReaderSize(dec, 1<<16)
		head, _ := br.Peek(512)
		if len(head) >= 262 && string(head[257:262]) == "ustar" {
			tr := tar.NewReader(br)
			for {
				if _, err := tr.Next(); err == io.EOF {
					return nil
				} else if err != nil {
					return fmt.Errorf("damaged: %w", err)
				}
				if _, err := io.Copy(io.Discard, tr); err != nil {
					return fmt.Errorf("damaged: %w", err)
				}
			}
		}
		if _, err := io.Copy(io.Discard, br); err != nil {
			return fmt.Errorf("damaged: %w", err)
		}
		return nil
	case KindZip:
		path, done, err := joinToTemp(archive, os.TempDir())
		if err != nil {
			return err
		}
		defer done()
		zr, err := zip.OpenReader(path)
		if err != nil {
			return x7test(ctx, archive, pw, cb)
		}
		defer zr.Close()
		var total int64
		for _, zf := range zr.File {
			if zf.Flags&1 != 0 || (zf.Method != zip.Store && zf.Method != zip.Deflate) {
				return x7test(ctx, archive, pw, cb)
			}
			total += int64(zf.UncompressedSize64)
		}
		c := newCounter(total, cb)
		for _, zf := range zr.File {
			rc, err := zf.Open()
			if err != nil {
				return fmt.Errorf("%s: %w", zf.Name, err)
			}
			c.set(zf.Name)
			_, err = io.Copy(io.Discard, &countingReader{ctx: ctx, r: rc, c: c})
			rc.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", zf.Name, err)
			}
		}
		return nil
	case KindDMG:
		if runtime.GOOS == "darwin" {
			args := []string{"verify", "-quiet"}
			if pw != "" {
				args = append(args, "-stdinpass")
			}
			return runTool(ctx, filepath.Dir(archive), pw, "hdiutil", append(args, archive)...)
		}
		return x7test(ctx, archive, pw, cb)
	case Kind7z, KindISO:
		return x7test(ctx, archive, pw, cb)
	default:
		tmp, err := os.MkdirTemp("", "pakket-test-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(tmp)
		return extractInto(ctx, f, archive, tmp, pw, o, cb)
	}
}

func x7test(ctx context.Context, archive, pw string, cb Progress) error {
	args := []string{"t", "-bsp1", "-bso1", "-bse2"}
	args = append(args, "--", Parts(archive)[0])
	c := newCounter(sizeOf(archive), cb)
	c.set(filepath.Base(archive))
	return run7z(ctx, filepath.Dir(archive), args, pw, pctTo(c))
}
