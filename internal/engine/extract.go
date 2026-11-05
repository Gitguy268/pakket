package engine

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/klauspost/compress/gzip"
	"golang.org/x/text/encoding"
	"golang.org/x/text/encoding/htmlindex"
	"golang.org/x/text/encoding/ianaindex"
)

// Extract unpacks an archive and returns where the result went: the single
// item it contained, the new folder, or the destination folder.
func Extract(ctx context.Context, archive string, o ExtractOptions, cb Progress) (string, error) {
	archive, err := filepath.Abs(archive)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(archive); err != nil {
		return "", err
	}
	f, err := Detect(archive)
	if err != nil {
		return "", err
	}
	dest := o.DestDir
	if dest == "" {
		dest = filepath.Dir(archive)
	}
	if dest, err = filepath.Abs(dest); err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(dest, ".pakket-x-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)

	if err := extractWithPasswords(ctx, f, archive, tmp, o, cb); err != nil {
		return "", err
	}
	if o.Nested {
		unwrapInnerTar(ctx, tmp, o)
	}
	result, placed, err := place(tmp, dest, StripExt(filepath.Base(archive)), o)
	if err != nil {
		return "", err
	}
	if o.Quarantine {
		if q := getQuarantine(archive); q != nil {
			for _, p := range placed {
				applyQuarantine(p, q)
			}
		}
	}
	if o.DeleteAfter {
		if err := Trash(Parts(archive)...); err != nil {
			return result, fmt.Errorf("extracted, but the archive could not be moved to the Trash: %w", err)
		}
	}
	return result, nil
}

// passwordsFor lists the passwords to try: none first, then the given ones.
func passwordsFor(o ExtractOptions) []string {
	pws := []string{""}
	seen := map[string]bool{"": true}
	for _, p := range append([]string{o.Password}, o.Passwords...) {
		if !seen[p] {
			seen[p] = true
			pws = append(pws, p)
		}
	}
	return pws
}

func extractWithPasswords(ctx context.Context, f Format, archive, tmp string, o ExtractOptions, cb Progress) error {
	var last error
	for _, pw := range passwordsFor(o) {
		if pw == "" && o.Password != "" && f.Kind == KindDMG {
			continue // avoid an interactive prompt from hdiutil
		}
		clearDir(tmp)
		err := extractInto(ctx, f, archive, tmp, pw, o, cb)
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

func clearDir(d string) {
	ents, _ := os.ReadDir(d)
	for _, e := range ents {
		os.RemoveAll(filepath.Join(d, e.Name()))
	}
}

func extractInto(ctx context.Context, f Format, archive, tmp, pw string, o ExtractOptions, cb Progress) error {
	switch f.Kind {
	case KindTar:
		return goUntarFile(ctx, f.Codec, archive, tmp, cb)
	case KindStream:
		return goStream(ctx, f, archive, tmp, o, cb)
	case KindZip:
		return goUnzip(ctx, archive, tmp, pw, o, cb)
	case Kind7z, KindISO:
		return x7z(ctx, archive, tmp, pw, o, cb)
	case KindDMG:
		if runtime.GOOS == "darwin" {
			return xDMG(ctx, archive, tmp, pw, cb)
		}
		return x7z(ctx, archive, tmp, pw, o, cb)
	case KindAAR:
		return runTool(ctx, tmp, "", "aa", "extract", "-i", archive, "-d", tmp)
	case KindXip:
		return runTool(ctx, tmp, "", "xip", "--expand", archive)
	case KindLrzip:
		name := filepath.Join(tmp, StripExt(filepath.Base(archive)))
		if err := runTool(ctx, tmp, "", "lrzip", "-q", "-d", "-f", "-o", name, archive); err != nil {
			return err
		}
		return unwrapFile(ctx, name, tmp, o)
	case KindWpress:
		return xWpress(ctx, archive, tmp, cb)
	case KindUnar:
		args := []string{"-q", "-f", "-D", "-o", tmp}
		if pw != "" {
			args = append(args, "-p", pw)
		}
		if o.Encoding != "" && o.Encoding != "auto" {
			args = append(args, "-e", o.Encoding)
		}
		return runTool(ctx, tmp, "", "unar", append(args, archive)...)
	case KindAge:
		return xAge(ctx, archive, tmp, pw, o, cb)
	}
	return fmt.Errorf("cannot extract %s", f.Name)
}

// safeJoin returns root/name, refusing names that escape root.
func safeJoin(root, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	clean := filepath.Clean("/" + name)
	if clean == "/" {
		return root, nil
	}
	p := filepath.Join(root, clean)
	if !strings.HasPrefix(p, root+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe path in archive: %q", name)
	}
	// No parent inside root may be a symlink: that would let an entry write outside.
	rel, _ := filepath.Rel(root, filepath.Dir(p))
	cur := root
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("unsafe path in archive (through a symlink): %q", name)
			}
		}
	}
	return p, nil
}

type dirTime struct {
	path string
	t    time.Time
}

func setDirTimes(ds []dirTime) {
	sort.Slice(ds, func(i, j int) bool { return len(ds[i].path) > len(ds[j].path) })
	for _, d := range ds {
		_ = os.Chtimes(d.path, d.t, d.t)
	}
}

func goUntarFile(ctx context.Context, codec, archive, tmp string, cb Progress) error {
	j, err := openJoined(archive)
	if err != nil {
		return err
	}
	defer j.Close()
	c := newCounter(j.size, cb)
	dec, err := newDecoder(codec, &countingReader{ctx: ctx, r: j, c: c})
	if err != nil {
		return err
	}
	defer dec.Close()
	return untar(ctx, dec, tmp, c)
}

func untar(ctx context.Context, r io.Reader, root string, c *counter) error {
	tr := tar.NewReader(r)
	var dirs []dirTime
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("damaged tar data: %w", err)
		}
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		p, err := safeJoin(root, h.Name)
		if err != nil {
			return err
		}
		if p == root {
			continue
		}
		c.set(h.Name)
		mode := os.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			_ = os.Chmod(p, mode|0o700)
			dirs = append(dirs, dirTime{p, h.ModTime})
		case tar.TypeReg, tar.TypeRegA, tar.TypeGNUSparse:
			if err := writeEntry(p, tr, mode, h.ModTime); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := safeJoin(root, h.Linkname)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Link(target, p); err != nil {
				return err
			}
		}
	}
	setDirTimes(dirs)
	return nil
}

func writeEntry(p string, r io.Reader, mode os.FileMode, mt time.Time) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if mode == 0 {
		mode = 0o644
	}
	os.Remove(p)
	out, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode|0o200)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_ = os.Chmod(p, mode)
	if !mt.IsZero() {
		_ = os.Chtimes(p, mt, mt)
	}
	return nil
}

// goStream unpacks a single compressed stream. If it holds a tar it is
// unpacked as one; a cpio (cpgz) is handed to 7-Zip.
func goStream(ctx context.Context, f Format, archive, tmp string, o ExtractOptions, cb Progress) error {
	j, err := openJoined(archive)
	if err != nil {
		return err
	}
	defer j.Close()
	c := newCounter(j.size, cb)
	cr := &countingReader{ctx: ctx, r: j, c: c}
	name := StripExt(filepath.Base(archive))
	var dec io.ReadCloser
	if f.Codec == "gzip" {
		gz, err := gzip.NewReader(bufio.NewReader(cr))
		if err != nil {
			return fmt.Errorf("not a valid gzip file: %w", err)
		}
		if gz.Name != "" && !o.IgnoreGzName {
			name = filepath.Base(gz.Name)
		}
		dec = gz
	} else {
		if dec, err = newDecoder(f.Codec, cr); err != nil {
			return err
		}
	}
	defer dec.Close()
	br := bufio.NewReaderSize(dec, 1<<16)
	head, _ := br.Peek(512)
	if len(head) >= 262 && string(head[257:262]) == "ustar" {
		return untar(ctx, br, tmp, c)
	}
	if name == "" || name == "." || name == filepath.Base(archive) {
		name = filepath.Base(archive) + ".out"
	}
	p, err := safeJoin(tmp, name)
	if err != nil {
		return err
	}
	c.set(name)
	if err := writeEntry(p, br, 0o644, time.Time{}); err != nil {
		return fmt.Errorf("damaged %s data: %w", f.Name, err)
	}
	return unwrapFile(ctx, p, tmp, o)
}

// unwrapFile unpacks an inner tar or cpio left by a stream decoder.
func unwrapFile(ctx context.Context, p, tmp string, o ExtractOptions) error {
	inner, err := Detect(p)
	if err != nil {
		return nil
	}
	switch {
	case inner.Kind == KindTar && inner.Codec == "":
		err = goUntarFile(ctx, "", p, tmp, nil)
	case inner.Name == "cpio":
		err = x7z(ctx, p, tmp, "", o, nil)
	default:
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(p)
}

// unwrapInnerTar unpacks a lone .tar left after extraction (7z of a tar, ...).
func unwrapInnerTar(ctx context.Context, tmp string, o ExtractOptions) {
	ents, _ := os.ReadDir(tmp)
	if len(ents) != 1 || ents[0].IsDir() {
		return
	}
	p := filepath.Join(tmp, ents[0].Name())
	if f, err := Detect(p); err == nil && f.Kind == KindTar {
		sub, err := os.MkdirTemp(tmp, ".inner-")
		if err != nil {
			return
		}
		if goUntarFile(ctx, f.Codec, p, sub, nil) != nil {
			os.RemoveAll(sub)
			return
		}
		os.Remove(p)
		inner, _ := os.ReadDir(sub)
		for _, e := range inner {
			_ = os.Rename(filepath.Join(sub, e.Name()), filepath.Join(tmp, e.Name()))
		}
		os.RemoveAll(sub)
	}
}

// decoderFor returns a decoder for zip names that are not UTF-8.
func decoderFor(name string) (*encoding.Decoder, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	alias := map[string]string{"cp437": "IBM437", "dos": "IBM437", "cp850": "IBM850", "cp866": "IBM866", "sjis": "shift_jis", "cp932": "shift_jis"}
	if a, ok := alias[n]; ok {
		n = a
	}
	if e, err := htmlindex.Get(n); err == nil {
		return e.NewDecoder(), nil
	}
	if e, err := ianaindex.IANA.Encoding(n); err == nil && e != nil {
		return e.NewDecoder(), nil
	}
	return nil, fmt.Errorf("unknown encoding %q", name)
}

func zipName(f *zip.File, dec *encoding.Decoder, forced bool) string {
	if !f.NonUTF8 {
		return f.Name
	}
	if !forced && utf8.ValidString(f.Name) {
		return f.Name // macOS Archive Utility writes UTF-8 without the flag
	}
	if dec != nil {
		if s, err := dec.String(f.Name); err == nil {
			return s
		}
	}
	return f.Name
}

func goUnzip(ctx context.Context, archive, tmp, pw string, o ExtractOptions, cb Progress) error {
	path, done, err := joinToTemp(archive, tmp)
	if err != nil {
		return err
	}
	defer done()
	zr, err := zip.OpenReader(path)
	if err != nil {
		// Damaged or unusual zips: 7-Zip is more forgiving.
		return x7z(ctx, archive, tmp, pw, o, cb)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Flags&1 != 0 || (f.Method != zip.Store && f.Method != zip.Deflate) {
			zr.Close()
			return x7z(ctx, archive, tmp, pw, o, cb)
		}
	}
	forced := o.Encoding != "" && o.Encoding != "auto"
	enc := "IBM437"
	if forced {
		enc = o.Encoding
	}
	dec, err := decoderFor(enc)
	if err != nil {
		return err
	}
	var total int64
	for _, f := range zr.File {
		total += int64(f.UncompressedSize64)
	}
	c := newCounter(total, cb)
	var dirs []dirTime
	for _, f := range zr.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := zipName(f, dec, forced)
		if strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		p, err := safeJoin(tmp, name)
		if err != nil {
			return err
		}
		if p == tmp {
			continue
		}
		c.set(name)
		mode := f.Mode()
		switch {
		case mode.IsDir() || strings.HasSuffix(name, "/"):
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{p, f.Modified})
		case mode&os.ModeSymlink != 0:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			b, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return err
			}
			os.Remove(p)
			if err := os.Symlink(string(b), p); err != nil {
				return err
			}
		default:
			rc, err := f.Open()
			if err != nil {
				return err
			}
			err = writeEntry(p, &countingReader{ctx: ctx, r: rc, c: c}, mode.Perm(), f.Modified)
			rc.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}
	setDirTimes(dirs)
	return nil
}

var codepages = map[string]string{"shift_jis": "932", "sjis": "932", "cp932": "932", "gbk": "936", "gb2312": "936", "big5": "950",
	"euc-kr": "949", "cp949": "949", "cp437": "437", "cp850": "850", "cp866": "866", "windows-1250": "1250",
	"windows-1251": "1251", "windows-1252": "1252", "windows-1253": "1253", "windows-1254": "1254", "windows-1255": "1255", "windows-1256": "1256"}

func x7z(ctx context.Context, archive, tmp, pw string, o ExtractOptions, cb Progress) error {
	first := Parts(archive)[0]
	args := []string{"x", "-y", "-bsp1", "-bso1", "-bse2", "-o" + tmp}
	if cp, ok := codepages[strings.ToLower(o.Encoding)]; ok {
		args = append(args, "-mcp="+cp)
	}
	args = append(args, "--", first)
	var size int64
	for _, p := range Parts(archive) {
		if st, err := os.Stat(p); err == nil {
			size += st.Size()
		}
	}
	c := newCounter(size, cb)
	c.set(filepath.Base(archive))
	return run7z(ctx, tmp, args, pw, pctTo(c))
}

func xDMG(ctx context.Context, archive, tmp, pw string, cb Progress) error {
	path, done, err := joinToTemp(archive, filepath.Dir(tmp))
	if err != nil {
		return err
	}
	defer done()
	encrypted := false
	if out, err := exec.CommandContext(ctx, "hdiutil", "isencrypted", path).CombinedOutput(); err == nil {
		encrypted = strings.Contains(string(out), "encrypted: YES")
	}
	if encrypted && pw == "" {
		return ErrPassword // never let hdiutil open a password dialog
	}
	mnt, err := os.MkdirTemp("", "pakket-mnt-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)
	args := []string{"attach", "-quiet", "-nobrowse", "-readonly", "-noautoopen", "-noverify", "-mountpoint", mnt}
	if encrypted {
		args = append(args, "-stdinpass")
	}
	args = append(args, path)
	if err := runTool(ctx, tmp, pw, "hdiutil", args...); err != nil {
		if encrypted {
			return ErrPassword
		}
		return err
	}
	defer runTool(context.Background(), tmp, "", "hdiutil", "detach", "-quiet", "-force", mnt)
	c := newCounter(0, cb)
	c.set(filepath.Base(archive))
	// ditto the mount as a whole: given a symlink as its source it would follow it.
	if err := runTool(ctx, tmp, "", "ditto", mnt, tmp); err != nil {
		return err
	}
	ents, _ := os.ReadDir(tmp)
	for _, e := range ents {
		if isMacJunk(e.Name()) || strings.HasPrefix(e.Name(), ".HFS+ Private Directory Data") || strings.HasPrefix(e.Name(), ".journal") {
			os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
	return nil
}

// xWpress reads All-in-One WP Migration backups: a 4377-byte header
// (name 255, size 14, mtime 12, folder 4096) before each file, then a zero header.
func xWpress(ctx context.Context, archive, tmp string, cb Progress) error {
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	st, _ := in.Stat()
	c := newCounter(st.Size(), cb)
	r := bufio.NewReader(&countingReader{ctx: ctx, r: in, c: c})
	hdr := make([]byte, 4377)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("damaged wpress file: %w", err)
		}
		cstr := func(b []byte) string { return strings.TrimRight(string(b), "\x00") }
		name, folder := cstr(hdr[:255]), cstr(hdr[281:])
		if name == "" {
			return nil
		}
		size, err := strconv.ParseInt(strings.TrimSpace(cstr(hdr[255:269])), 10, 64)
		if err != nil {
			return fmt.Errorf("damaged wpress header for %q", name)
		}
		mt, _ := strconv.ParseInt(strings.TrimSpace(cstr(hdr[269:281])), 10, 64)
		p, err := safeJoin(tmp, filepath.Join(folder, name))
		if err != nil {
			return err
		}
		c.set(name)
		if err := writeEntry(p, io.LimitReader(r, size), 0o644, time.Unix(mt, 0)); err != nil {
			return err
		}
	}
}

func ageIdentities(pw string, files []string) ([]age.Identity, error) {
	var ids []age.Identity
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			return nil, err
		}
		got, err := age.ParseIdentities(fh)
		fh.Close()
		if err != nil {
			return nil, fmt.Errorf("age identity %s: %w", f, err)
		}
		ids = append(ids, got...)
	}
	if pw != "" {
		id, err := age.NewScryptIdentity(pw)
		if err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, ErrPassword
	}
	return ids, nil
}

func xAge(ctx context.Context, archive, tmp, pw string, o ExtractOptions, cb Progress) error {
	ids, err := ageIdentities(pw, o.Identities)
	if err != nil {
		return err
	}
	in, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer in.Close()
	st, _ := in.Stat()
	c := newCounter(st.Size(), cb)
	br := bufio.NewReader(&countingReader{ctx: ctx, r: in, c: c})
	var src io.Reader = br
	if head, _ := br.Peek(len(magicAgeA)); string(head) == string(magicAgeA) {
		src = armor.NewReader(br)
	}
	dr, err := age.Decrypt(src, ids...)
	if err != nil {
		var nm *age.NoIdentityMatchError
		if errors.As(err, &nm) || strings.Contains(err.Error(), "incorrect passphrase") || strings.Contains(err.Error(), "no identity matched") {
			return ErrPassword
		}
		return err
	}
	name := strings.TrimSuffix(filepath.Base(archive), filepath.Ext(archive))
	p, err := safeJoin(tmp, name)
	if err != nil {
		return err
	}
	if err := writeEntry(p, dr, 0o600, time.Time{}); err != nil {
		return fmt.Errorf("decryption failed (damaged file?): %w", err)
	}
	// If the decrypted file is itself an archive, unpack it in place.
	inner, err := Detect(p)
	if err != nil || inner.Kind == KindAge {
		return nil
	}
	sub, err := os.MkdirTemp(tmp, ".inner-")
	if err != nil {
		return err
	}
	inO := o
	inO.Password, inO.Passwords = pw, nil
	if err := extractInto(ctx, inner, p, sub, pw, inO, nil); err != nil {
		os.RemoveAll(sub)
		return nil // keep the decrypted file as is
	}
	os.Remove(p)
	ents, _ := os.ReadDir(sub)
	for _, e := range ents {
		_ = os.Rename(filepath.Join(sub, e.Name()), filepath.Join(tmp, e.Name()))
	}
	return os.RemoveAll(sub)
}

// place moves extracted items from tmp to dest according to o.Folder. It
// returns the main result and every top-level path it created.
func place(tmp, dest, stem string, o ExtractOptions) (string, []string, error) {
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return "", nil, err
	}
	if len(ents) == 0 {
		return "", nil, errors.New("the archive is empty")
	}
	mode := o.Folder
	if mode == "" {
		mode = "smart"
	}
	if mode == "always" && len(ents) == 1 && ents[0].IsDir() && ents[0].Name() == stem {
		mode = "smart" // avoid "x/x"
	}
	switch mode {
	case "smart":
		if len(ents) == 1 {
			p, err := moveInto(filepath.Join(tmp, ents[0].Name()), filepath.Join(dest, ents[0].Name()), o.Overwrite)
			return p, []string{p}, err
		}
		fallthrough
	case "always":
		target := uniquePlain(filepath.Join(dest, stem))
		if err := os.Rename(tmp, target); err != nil {
			return "", nil, err
		}
		_ = os.Chmod(target, 0o755) // MkdirTemp made it 0700
		return target, []string{target}, nil
	case "never":
		var placed []string
		for _, e := range ents {
			p, err := moveInto(filepath.Join(tmp, e.Name()), filepath.Join(dest, e.Name()), o.Overwrite)
			if err != nil {
				return "", placed, err
			}
			placed = append(placed, p)
		}
		return dest, placed, nil
	}
	return "", nil, fmt.Errorf("unknown folder mode %q (smart, always, never)", o.Folder)
}

// uniquePlain is uniquePath for folder names (no archive extension logic).
func uniquePlain(p string) string {
	if !fileExists(p) {
		return p
	}
	for i := 2; ; i++ {
		c := fmt.Sprintf("%s %d", p, i)
		if !fileExists(c) {
			return c
		}
	}
}

func moveInto(src, dst, policy string) (string, error) {
	if fileExists(dst) {
		switch policy {
		case "overwrite":
			if err := os.RemoveAll(dst); err != nil {
				return "", err
			}
		case "skip":
			return dst, nil
		default:
			if st, err := os.Lstat(src); err == nil && st.IsDir() {
				dst = uniquePlain(dst)
			} else {
				dst = uniquePath(dst, false)
			}
		}
	}
	return dst, os.Rename(src, dst)
}
