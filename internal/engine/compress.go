package engine

import (
	"archive/tar"
	"archive/zip"
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

	"filippo.io/age"
	"github.com/klauspost/compress/flate"
)

// Compress makes one archive from all inputs, or one per input when
// o.Separate is set. It returns the files written (volumes included).
func Compress(ctx context.Context, inputs []string, o CompressOptions, cb Progress) ([]string, error) {
	if len(inputs) == 0 {
		return nil, errors.New("nothing to compress")
	}
	f, ok := ByName(o.Format)
	if !ok || !f.Create {
		return nil, fmt.Errorf("cannot create %q; try one of: %s", o.Format, strings.Join(CreateNames(), ", "))
	}
	if o.Password != "" && !f.Encrypt && f.Kind != KindAge {
		return nil, fmt.Errorf("%s archives cannot have a password; use 7z, zip, dmg or age", f.Name)
	}
	abs := make([]string, 0, len(inputs))
	for _, in := range inputs {
		a, err := filepath.Abs(in)
		if err != nil {
			return nil, err
		}
		if _, err := os.Lstat(a); err != nil {
			return nil, err
		}
		abs = append(abs, a)
	}
	if o.Separate && len(abs) > 1 {
		var all []string
		for _, in := range abs {
			one := o
			one.Name, one.Output = "", ""
			outs, err := compressOne(ctx, f, []string{in}, one, cb)
			all = append(all, outs...)
			if err != nil {
				return all, fmt.Errorf("%s: %w", filepath.Base(in), err)
			}
		}
		return all, nil
	}
	return compressOne(ctx, f, abs, o, cb)
}

func compressOne(ctx context.Context, f Format, inputs []string, o CompressOptions, cb Progress) (outs []string, err error) {
	// Folders and several files cannot go into a bare stream: tar them first.
	multi := len(inputs) > 1
	if !multi {
		if st, err := os.Stat(inputs[0]); err == nil && st.IsDir() {
			multi = true
		}
	}
	ext := f.Exts[0]
	if multi && (f.Kind == KindStream || f.Kind == KindLrzip || f.Kind == KindAge) {
		ext = ".tar" + ext
	}

	out := o.Output
	if out == "" {
		dest := o.DestDir
		if dest == "" {
			dest = filepath.Dir(inputs[0])
		}
		name := o.Name
		if name == "" {
			if len(inputs) == 1 {
				name = filepath.Base(inputs[0])
			} else {
				name = "Archive"
			}
		}
		out = filepath.Join(dest, name+ext)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	out = uniquePath(out, o.Overwrite)
	if o.SplitSize > 0 && !o.Overwrite && fileExists(out+".001") {
		stem, ext := StripExt(out), out[len(StripExt(out)):]
		for i := 2; fileExists(out) || fileExists(out+".001"); i++ {
			out = fmt.Sprintf("%s %d%s", stem, i, ext)
		}
	}
	if o.Overwrite {
		os.Remove(out)
	}

	c := newCounter(treeSize(inputs, &o), cb)
	defer func() {
		if err != nil {
			os.Remove(out)
			for _, p := range Parts(out + ".001") {
				os.Remove(p)
			}
			outs = nil
		}
	}()

	switch f.Kind {
	case KindTar, KindStream, KindAge:
		outs, err = writeGo(ctx, f, inputs, multi, out, o, c)
	case KindZip:
		if o.Password != "" {
			outs, err = with7z(ctx, "zip", inputs, out, o, c)
		} else {
			outs, err = writeGo(ctx, f, inputs, multi, out, o, c)
		}
	case Kind7z:
		outs, err = with7z(ctx, f.Name, inputs, out, o, c)
	case KindDMG:
		outs, err = makeDMG(ctx, inputs, out, o, c)
	case KindISO:
		outs, err = makeISO(ctx, inputs, out, o, c)
	case KindAAR:
		outs, err = makeAAR(ctx, inputs, out, o, c)
	case KindLrzip:
		outs, err = makeLrz(ctx, inputs, multi, out, o, c)
	default:
		err = fmt.Errorf("cannot create %s", f.Name)
	}
	if err != nil {
		return nil, err
	}
	// External tools other than 7-Zip do not split; cut their output here.
	if o.SplitSize > 0 && len(outs) == 1 && outs[0] == out {
		if st, _ := os.Stat(out); st != nil && st.Size() > o.SplitSize {
			if outs, err = splitFile(out, o.SplitSize); err != nil {
				return nil, err
			}
		}
	}
	if err = writeChecksum(o.Checksum, outs); err != nil {
		return outs, err
	}
	if o.ExportPassword && o.Password != "" {
		pw := strings.TrimSuffix(StripExt(out), string(filepath.Separator)) + ".txt"
		if err = os.WriteFile(uniquePath(pw, false), []byte(o.Password+"\n"), 0o600); err != nil {
			return outs, err
		}
	}
	if o.DeleteAfter {
		if err = Trash(inputs...); err != nil {
			return outs, fmt.Errorf("archive made, but the originals could not be moved to the Trash: %w", err)
		}
	}
	return outs, nil
}

func fileExists(p string) bool { _, err := os.Lstat(p); return err == nil }

// writeGo handles every format written in pure Go.
func writeGo(ctx context.Context, f Format, inputs []string, multi bool, out string, o CompressOptions, c *counter) ([]string, error) {
	var sink io.WriteCloser
	var parts func() []string
	if o.SplitSize > 0 {
		sw := newSplitWriter(out, o.SplitSize)
		sink, parts = sw, func() []string { return sw.parts }
	} else {
		fh, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err != nil {
			return nil, err
		}
		sink, parts = fh, func() []string { return []string{out} }
	}
	err := func() error {
		defer sink.Close()
		w := io.Writer(sink)
		if f.Kind == KindAge {
			rs, err := ageRecipients(o)
			if err != nil {
				return err
			}
			aw, err := age.Encrypt(w, rs...)
			if err != nil {
				return err
			}
			if multi {
				err = writeTar(ctx, aw, inputs, &o, c)
			} else {
				err = copyFile(ctx, aw, inputs[0], c)
			}
			if err != nil {
				return err
			}
			return aw.Close()
		}
		switch {
		case f.Kind == KindZip:
			return writeZip(ctx, w, inputs, &o, c)
		default:
			enc, err := newEncoder(f.Codec, w, o.Level)
			if err != nil {
				return err
			}
			if f.Kind == KindTar || multi {
				err = writeTar(ctx, enc, inputs, &o, c)
			} else {
				err = copyFile(ctx, enc, inputs[0], c)
			}
			if err != nil {
				enc.Close()
				return err
			}
			return enc.Close()
		}
	}()
	if err != nil {
		for _, p := range parts() {
			os.Remove(p)
		}
		return nil, err
	}
	return parts(), nil
}

func ageRecipients(o CompressOptions) ([]age.Recipient, error) {
	if len(o.AgeRecipients) > 0 {
		var rs []age.Recipient
		for _, r := range o.AgeRecipients {
			var src io.Reader
			if fileExists(r) {
				fh, err := os.Open(r)
				if err != nil {
					return nil, err
				}
				defer fh.Close()
				src = fh
			} else {
				src = strings.NewReader(r)
			}
			got, err := age.ParseRecipients(src)
			if err != nil {
				return nil, fmt.Errorf("age recipient %q: %w", r, err)
			}
			rs = append(rs, got...)
		}
		return rs, nil
	}
	if o.Password == "" {
		return nil, errors.New("age needs a password or a recipient (-r)")
	}
	r, err := age.NewScryptRecipient(o.Password)
	if err != nil {
		return nil, err
	}
	return []age.Recipient{r}, nil
}

func copyFile(ctx context.Context, w io.Writer, path string, c *counter) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	c.set(filepath.Base(path))
	_, err = io.Copy(w, &countingReader{ctx: ctx, r: in, c: c})
	return err
}

// walkInputs visits every item under the inputs, with its archive name.
func walkInputs(inputs []string, o *CompressOptions, fn func(path, name string, info os.FileInfo) error) error {
	for _, in := range inputs {
		parent := filepath.Dir(in)
		err := filepath.Walk(in, func(p string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if p != in && excluded(info.Name(), o) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(parent, p)
			if err != nil {
				return err
			}
			return fn(p, filepath.ToSlash(rel), info)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func writeTar(ctx context.Context, w io.Writer, inputs []string, o *CompressOptions, c *counter) error {
	tw := tar.NewWriter(w)
	err := walkInputs(inputs, o, func(p, name string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		link := ""
		mode := info.Mode()
		switch {
		case mode&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			link = l
		case mode.IsDir(), mode.IsRegular():
		default:
			return nil // sockets, devices, pipes
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = name
		if mode.IsDir() {
			hdr.Name += "/"
		}
		hdr.Format = tar.FormatPAX
		hdr.Uname, hdr.Gname = "", ""
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if mode.IsRegular() {
			return copyFile(ctx, tw, p, c)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

func writeZip(ctx context.Context, w io.Writer, inputs []string, o *CompressOptions, c *counter) error {
	zw := zip.NewWriter(w)
	level := clampLevel(o.Level)
	zw.RegisterCompressor(zip.Deflate, func(out io.Writer) (io.WriteCloser, error) {
		return flate.NewWriter(out, level)
	})
	err := walkInputs(inputs, o, func(p, name string, info os.FileInfo) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		mode := info.Mode()
		if !mode.IsDir() && !mode.IsRegular() && mode&os.ModeSymlink == 0 {
			return nil
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = name
		hdr.Method = zip.Deflate
		if level == 0 || mode.IsDir() || mode&os.ModeSymlink != 0 {
			hdr.Method = zip.Store
		}
		if mode.IsDir() {
			hdr.Name += "/"
		}
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		switch {
		case mode&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_, err = io.WriteString(fw, l)
			return err
		case mode.IsRegular():
			return copyFile(ctx, fw, p, c)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

func pctTo(c *counter) func(int) {
	return func(p int) {
		if c == nil {
			return
		}
		want := c.total * int64(p) / 100
		if d := want - c.done.Load(); d > 0 {
			c.add(d)
		}
	}
}

// with7z creates 7z, wim and encrypted zip archives through 7-Zip.
func with7z(ctx context.Context, typ string, inputs []string, out string, o CompressOptions, c *counter) ([]string, error) {
	args := []string{"a", "-t" + typ, "-bsp1", "-bso1", "-bse2", "-y", "-snl"}
	if typ != "wim" {
		args = append(args, "-mx="+strconv.Itoa(clampLevel(o.Level)))
	}
	if typ == "7z" {
		if o.Solid {
			args = append(args, "-ms=on")
		} else {
			args = append(args, "-ms=off")
		}
	}
	if o.Threads > 0 {
		args = append(args, "-mmt="+strconv.Itoa(o.Threads))
	}
	if o.Password != "" {
		args = append(args, "-p")
		switch typ {
		case "7z":
			if o.EncryptNames {
				args = append(args, "-mhe=on")
			}
		case "zip":
			if o.ZipLegacy {
				args = append(args, "-mem=ZipCrypto")
			} else {
				args = append(args, "-mem=AES256")
			}
		}
	}
	if o.SplitSize > 0 {
		args = append(args, "-v"+strconv.FormatInt(o.SplitSize, 10)+"b")
	}
	if o.ExcludeMac {
		for _, x := range []string{".DS_Store", "._*", "__MACOSX", ".Spotlight-V100", ".fseventsd", ".Trashes", ".TemporaryItems"} {
			args = append(args, "-xr!"+x)
		}
	}
	for _, x := range o.Exclude {
		args = append(args, "-xr!"+x)
	}
	args = append(args, "--", out)
	args = append(args, inputs...)
	c.set(filepath.Base(out))
	if err := run7z(ctx, filepath.Dir(out), args, o.Password, pctTo(c)); err != nil {
		return nil, err
	}
	if o.SplitSize > 0 {
		return Parts(out + ".001"), nil
	}
	return []string{out}, nil
}

// stage builds a folder holding the inputs, cloned (APFS) or copied, with
// Mac junk removed. A single folder input becomes the stage root itself.
func stage(inputs []string, near string, o *CompressOptions) (string, func(), error) {
	tmp, err := os.MkdirTemp(near, ".pakket-stage-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	root := tmp
	if len(inputs) == 1 {
		if st, err := os.Stat(inputs[0]); err == nil && st.IsDir() {
			root = filepath.Join(tmp, filepath.Base(inputs[0]))
		}
	}
	for _, in := range inputs {
		dst := filepath.Join(tmp, filepath.Base(in))
		if err := cloneTree(in, dst); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && p != root && excluded(info.Name(), o) {
			os.RemoveAll(p)
			if info.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	return root, cleanup, nil
}

func cloneTree(src, dst string) error {
	if runtime.GOOS == "darwin" {
		if err := exec.Command("cp", "-cRp", src, dst).Run(); err == nil {
			return nil
		}
		os.RemoveAll(dst)
	}
	out, err := exec.Command("cp", "-Rp", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("copy %s: %s", filepath.Base(src), strings.TrimSpace(string(out)))
	}
	return nil
}

func volName(o CompressOptions, out string) string {
	if o.VolumeName != "" {
		return o.VolumeName
	}
	return StripExt(filepath.Base(out))
}

func makeDMG(ctx context.Context, inputs []string, out string, o CompressOptions, c *counter) ([]string, error) {
	if _, err := need("hdiutil"); err != nil {
		return nil, err
	}
	src, cleanup, err := stage(inputs, filepath.Dir(out), &o)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	format := "UDZO"
	args := []string{"create", "-quiet", "-srcfolder", src, "-volname", volName(o, out), "-fs", "HFS+"}
	switch l := clampLevel(o.Level); {
	case l == 0:
		format = "UDRO"
	case l <= 3:
		args = append(args, "-imagekey", "zlib-level="+strconv.Itoa(l*3))
	case l <= 6:
		format = "ULFO"
	default:
		format = "ULMO"
	}
	args = append(args, "-format", format)
	if o.Password != "" {
		args = append(args, "-encryption", "AES-256", "-stdinpass")
	}
	args = append(args, out)
	c.set(filepath.Base(out))
	if err := runTool(ctx, filepath.Dir(out), o.Password, "hdiutil", args...); err != nil {
		return nil, err
	}
	c.add(c.total)
	return []string{out}, nil
}

func makeISO(ctx context.Context, inputs []string, out string, o CompressOptions, c *counter) ([]string, error) {
	if _, err := need("hdiutil"); err != nil {
		return nil, err
	}
	src, cleanup, err := stage(inputs, filepath.Dir(out), &o)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	c.set(filepath.Base(out))
	tmp := out + ".pakket-tmp"
	if err := runTool(ctx, filepath.Dir(out), "", "hdiutil", "makehybrid", "-quiet", "-iso", "-joliet", "-udf",
		"-default-volume-name", volName(o, out), "-o", tmp, src); err != nil {
		os.Remove(tmp)
		os.Remove(tmp + ".iso")
		return nil, err
	}
	// hdiutil may add .iso to the name it was given.
	if !fileExists(tmp) && fileExists(tmp+".iso") {
		tmp += ".iso"
	}
	if err := os.Rename(tmp, out); err != nil {
		return nil, err
	}
	c.add(c.total)
	return []string{out}, nil
}

func makeAAR(ctx context.Context, inputs []string, out string, o CompressOptions, c *counter) ([]string, error) {
	if _, err := need("aa"); err != nil {
		return nil, err
	}
	src, cleanup, err := stage(inputs, filepath.Dir(out), &o)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	algo := "lzfse"
	switch l := clampLevel(o.Level); {
	case l == 0:
		algo = "raw"
	case l <= 2:
		algo = "lz4"
	case l <= 6:
		algo = "lzfse"
	default:
		algo = "lzma"
	}
	args := []string{"archive", "-o", out, "-a", algo}
	// A single folder keeps its name inside the archive.
	if len(inputs) == 1 && src != filepath.Dir(src) && filepath.Base(src) == filepath.Base(inputs[0]) {
		args = append(args, "-d", filepath.Dir(src), "-subdir", filepath.Base(src))
	} else {
		args = append(args, "-d", src)
	}
	c.set(filepath.Base(out))
	if err := runTool(ctx, filepath.Dir(out), "", "aa", args...); err != nil {
		return nil, err
	}
	c.add(c.total)
	return []string{out}, nil
}

func makeLrz(ctx context.Context, inputs []string, multi bool, out string, o CompressOptions, c *counter) ([]string, error) {
	if _, err := need("lrzip"); err != nil {
		return nil, err
	}
	src := inputs[0]
	if multi {
		tmp, err := os.CreateTemp(filepath.Dir(out), ".pakket-*.tar")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		if err := writeTar(ctx, tmp, inputs, &o, c); err != nil {
			tmp.Close()
			return nil, err
		}
		tmp.Close()
		src = tmp.Name()
	}
	lvl := clampLevel(o.Level)
	if lvl == 0 {
		lvl = 1
	}
	if err := runTool(ctx, filepath.Dir(out), "", "lrzip", "-q", "-f", "-L", strconv.Itoa(lvl), "-o", out, src); err != nil {
		return nil, err
	}
	return []string{out}, nil
}
