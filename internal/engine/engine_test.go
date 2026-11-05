package engine

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// makeTree builds a small folder with a unicode name, a nested folder,
// an empty folder, a symlink and Mac junk that should be left out.
func makeTree(t *testing.T, root string) string {
	t.Helper()
	d := filepath.Join(root, "Fotoalbum é")
	must(t, os.MkdirAll(filepath.Join(d, "sub", "deeper"), 0o755))
	must(t, os.MkdirAll(filepath.Join(d, "empty"), 0o755))
	must(t, os.WriteFile(filepath.Join(d, "readme.txt"), []byte(strings.Repeat("hello pakket\n", 500)), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "sub", "日本語.txt"), []byte("konnichiwa"), 0o644))
	must(t, os.WriteFile(filepath.Join(d, "sub", "deeper", "bin.dat"), bytes.Repeat([]byte{0, 1, 2, 3, 250}, 40000), 0o755))
	must(t, os.WriteFile(filepath.Join(d, ".DS_Store"), []byte("junk"), 0o644))
	must(t, os.Symlink("readme.txt", filepath.Join(d, "link.txt")))
	return d
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// snapshot maps relative paths to contents ("<dir>", "-> target").
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	m := map[string]string{}
	must(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			l, _ := os.Readlink(p)
			m[rel] = "-> " + l
		case info.IsDir():
			m[rel] = "<dir>"
		default:
			b, _ := os.ReadFile(p)
			m[rel] = string(b)
		}
		return nil
	}))
	return m
}

func compare(t *testing.T, want, got map[string]string, skip func(string) bool) {
	t.Helper()
	var keys []string
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if skip != nil && skip(k) {
			continue
		}
		if g, ok := got[k]; !ok {
			t.Errorf("missing %q", k)
		} else if g != want[k] {
			t.Errorf("%q differs", k)
		}
	}
	if _, ok := got[".DS_Store"]; ok {
		t.Errorf(".DS_Store should have been left out")
	}
}

func available(f Format) bool {
	if f.Needs == "" {
		return true
	}
	_, ok := Tool(f.Needs)
	return ok
}

func TestRoundTripAllFormats(t *testing.T) {
	ctx := context.Background()
	for _, f := range Formats {
		if !f.Create {
			continue
		}
		f := f
		t.Run(f.Name, func(t *testing.T) {
			if !available(f) {
				t.Skipf("%s not installed", f.Needs)
			}
			tmp := t.TempDir()
			src := makeTree(t, tmp)
			want := snapshot(t, src)
			delete(want, ".DS_Store")
			pw := ""
			if f.Kind == KindAge {
				pw = "correct horse"
			}
			outs, err := Compress(ctx, []string{src}, CompressOptions{Format: f.Name, Level: 5, ExcludeMac: true, Solid: true, Password: pw, DestDir: tmp}, nil)
			must(t, err)
			if len(outs) != 1 {
				t.Fatalf("outputs: %v", outs)
			}
			got, err := Detect(outs[0])
			must(t, err)
			if got.Kind != f.Kind && !(f.Kind == KindStream && got.Kind == KindTar) && !(f.Kind == KindAge) {
				t.Errorf("detected %s as %s", outs[0], got.Name)
			}
			must(t, Test(ctx, outs[0], ExtractOptions{Password: pw}, nil))
			es, err := List(ctx, outs[0], ExtractOptions{Password: pw})
			must(t, err)
			if len(es) == 0 {
				t.Errorf("empty listing")
			}
			dest := filepath.Join(tmp, "out")
			res, err := Extract(ctx, outs[0], ExtractOptions{DestDir: dest, Password: pw, Nested: true}, nil)
			must(t, err)
			// Disk images hold the folder's contents at their root.
			if f.Kind == KindDMG || f.Kind == KindISO {
				if filepath.Base(res) != "out" && filepath.Base(res) != "Fotoalbum é" {
					t.Logf("result %s", res)
				}
			} else if filepath.Base(res) != "Fotoalbum é" {
				t.Fatalf("result %q, want the folder itself (smart folder)", res)
			}
			skip := func(k string) bool {
				switch f.Kind {
				case KindISO:
					return k == "link.txt" || k == "empty" // ISO9660 has no symlinks via makehybrid
				case Kind7z:
					return f.Name == "wim" && k == "link.txt"
				}
				return false
			}
			compare(t, want, snapshot(t, res), skip)
		})
	}
}

func TestSingleFileStreams(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"gz", "bz2", "xz", "zst", "br", "lz4", "lz", "lzma"} {
		t.Run(name, func(t *testing.T) {
			tmp := t.TempDir()
			p := filepath.Join(tmp, "notes.txt")
			must(t, os.WriteFile(p, []byte(strings.Repeat("abc", 10000)), 0o644))
			outs, err := Compress(ctx, []string{p}, CompressOptions{Format: name, Level: 9}, nil)
			must(t, err)
			if filepath.Base(outs[0]) != "notes.txt."+strings.TrimPrefix(mustFormat(t, name).Exts[0], ".") {
				t.Errorf("name %s", outs[0])
			}
			res, err := Extract(ctx, outs[0], ExtractOptions{DestDir: filepath.Join(tmp, "x")}, nil)
			must(t, err)
			b, err := os.ReadFile(res)
			must(t, err)
			if string(b) != strings.Repeat("abc", 10000) || filepath.Base(res) != "notes.txt" {
				t.Errorf("got %s", res)
			}
		})
	}
}

func mustFormat(t *testing.T, n string) Format {
	f, ok := ByName(n)
	if !ok {
		t.Fatal(n)
	}
	return f
}

func TestPasswords(t *testing.T) {
	if _, ok := Tool("7zz"); !ok {
		t.Skip("7zz not installed")
	}
	ctx := context.Background()
	cases := []struct {
		format string
		legacy bool
	}{{"7z", false}, {"zip", false}, {"zip", true}}
	if runtime.GOOS == "darwin" {
		cases = append(cases, struct {
			format string
			legacy bool
		}{"dmg", false})
	}
	for _, c := range cases {
		t.Run(c.format, func(t *testing.T) {
			tmp := t.TempDir()
			src := makeTree(t, tmp)
			outs, err := Compress(ctx, []string{src}, CompressOptions{Format: c.format, Password: "s3cret!", EncryptNames: true, ZipLegacy: c.legacy, ExcludeMac: true, Level: 5}, nil)
			must(t, err)
			_, err = Extract(ctx, outs[0], ExtractOptions{DestDir: filepath.Join(tmp, "bad"), Password: "nope"}, nil)
			if !errors.Is(err, ErrPassword) {
				t.Fatalf("wrong password: got %v, want ErrPassword", err)
			}
			res, err := Extract(ctx, outs[0], ExtractOptions{DestDir: filepath.Join(tmp, "good"), Passwords: []string{"nope", "s3cret!"}}, nil)
			must(t, err)
			b, err := os.ReadFile(filepath.Join(res, "sub", "日本語.txt"))
			must(t, err)
			if string(b) != "konnichiwa" {
				t.Errorf("content %q", b)
			}
		})
	}
}

func TestZipAESReadableBy7zip(t *testing.T) {
	bin, ok := Tool("7zz")
	if !ok {
		t.Skip("7zz not installed")
	}
	tmp := t.TempDir()
	src := makeTree(t, tmp)
	outs, err := Compress(context.Background(), []string{src}, CompressOptions{Format: "zip", Password: "pw", ExcludeMac: true}, nil)
	must(t, err)
	out, err := exec.Command(bin, "l", "-slt", "-ppw", outs[0]).CombinedOutput()
	must(t, err)
	if !strings.Contains(string(out), "AES-256") {
		t.Errorf("zip is not AES-256:\n%s", out)
	}
}

func TestSplitVolumes(t *testing.T) {
	ctx := context.Background()
	for _, name := range []string{"zip", "tar.zst", "7z"} {
		t.Run(name, func(t *testing.T) {
			if !available(mustFormat(t, name)) {
				t.Skip()
			}
			tmp := t.TempDir()
			src := filepath.Join(tmp, "big")
			must(t, os.MkdirAll(src, 0o755))
			rnd := make([]byte, 300<<10)
			_, _ = rand.Read(rnd)
			must(t, os.WriteFile(filepath.Join(src, "a.bin"), rnd, 0o644))
			outs, err := Compress(ctx, []string{src}, CompressOptions{Format: name, Level: 0, SplitSize: 100 << 10}, nil)
			must(t, err)
			if len(outs) < 3 || !strings.HasSuffix(outs[0], ".001") {
				t.Fatalf("volumes: %v", outs)
			}
			res, err := Extract(ctx, outs[1], ExtractOptions{DestDir: filepath.Join(tmp, "x")}, nil)
			must(t, err)
			b, err := os.ReadFile(filepath.Join(res, "a.bin"))
			must(t, err)
			if !bytes.Equal(b, rnd) {
				t.Error("content differs")
			}
		})
	}
}

func TestSeparateAndDelete(t *testing.T) {
	tmp := t.TempDir()
	var ins []string
	for _, n := range []string{"a.txt", "b.txt"} {
		p := filepath.Join(tmp, n)
		must(t, os.WriteFile(p, []byte(n), 0o644))
		ins = append(ins, p)
	}
	outs, err := Compress(context.Background(), ins, CompressOptions{Format: "zip", Separate: true}, nil)
	must(t, err)
	if len(outs) != 2 || filepath.Base(outs[0]) != "a.txt.zip" || filepath.Base(outs[1]) != "b.txt.zip" {
		t.Errorf("outputs %v", outs)
	}
	// A second run must not overwrite.
	outs2, err := Compress(context.Background(), ins[:1], CompressOptions{Format: "zip"}, nil)
	must(t, err)
	if filepath.Base(outs2[0]) != "a.txt 2.zip" {
		t.Errorf("unique name: %s", outs2[0])
	}
}

func TestFolderModes(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	a, b := filepath.Join(tmp, "a.txt"), filepath.Join(tmp, "b.txt")
	must(t, os.WriteFile(a, []byte("a"), 0o644))
	must(t, os.WriteFile(b, []byte("b"), 0o644))
	outs, err := Compress(ctx, []string{a, b}, CompressOptions{Format: "tar.gz", Name: "two"}, nil)
	must(t, err)
	res, err := Extract(ctx, outs[0], ExtractOptions{DestDir: filepath.Join(tmp, "s")}, nil)
	must(t, err)
	if filepath.Base(res) != "two" {
		t.Errorf("smart: %s", res)
	}
	res, err = Extract(ctx, outs[0], ExtractOptions{DestDir: filepath.Join(tmp, "n"), Folder: "never"}, nil)
	must(t, err)
	if _, err := os.Stat(filepath.Join(res, "a.txt")); err != nil {
		t.Errorf("never: %v", err)
	}
	// Extracting twice keeps both copies.
	res2, err := Extract(ctx, outs[0], ExtractOptions{DestDir: filepath.Join(tmp, "s")}, nil)
	must(t, err)
	if filepath.Base(res2) != "two 2" {
		t.Errorf("second extract: %s", res2)
	}
}

func TestRejectsPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	// zip with ../ entry
	zp := filepath.Join(tmp, "evil.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../../escaped.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	dest := filepath.Join(tmp, "d")
	_, err := Extract(context.Background(), zp, ExtractOptions{DestDir: dest}, nil)
	if err == nil {
		// Clean() maps ../../x to /x inside dest: allowed only if it stayed inside.
		if _, err := os.Stat(filepath.Join(tmp, "escaped.txt")); err == nil {
			t.Fatal("zip entry escaped the destination")
		}
	}
	// tar with a symlink, then a file written through it
	tp := filepath.Join(tmp, "evil.tar")
	tf, _ := os.Create(tp)
	tw := tar.NewWriter(tf)
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: tmp})
	tw.WriteHeader(&tar.Header{Name: "link/pwned.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.Close()
	tf.Close()
	_, err = Extract(context.Background(), tp, ExtractOptions{DestDir: filepath.Join(tmp, "t")}, nil)
	if err == nil {
		t.Error("expected an error for a write through a symlink")
	}
	if _, err := os.Stat(filepath.Join(tmp, "pwned.txt")); err == nil {
		t.Fatal("tar entry escaped through a symlink")
	}
}

// The same symlink trick, sent through 7-Zip (used for 7z, rar, iso, ...).
func TestRejectsSymlinkEscapeVia7zip(t *testing.T) {
	if _, ok := Tool("7zz"); !ok {
		t.Skip("7zz not installed")
	}
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "outside")
	must(t, os.MkdirAll(outside, 0o755))
	tp := filepath.Join(tmp, "evil.tar")
	tf, _ := os.Create(tp)
	tw := tar.NewWriter(tf)
	tw.WriteHeader(&tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside})
	tw.WriteHeader(&tar.Header{Name: "link/pwned.txt", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.Close()
	tf.Close()
	dest := filepath.Join(tmp, "d")
	must(t, os.MkdirAll(dest, 0o755))
	_ = x7z(context.Background(), tp, dest, "", ExtractOptions{}, nil)
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatal("7-Zip wrote through a symlink outside the destination")
	}
}

func TestInteropWithSystemTools(t *testing.T) {
	ctx := context.Background()
	tools := map[string][]string{
		"gz":  {"gzip", "-t"},
		"bz2": {"bzip2", "-t"},
		"xz":  {"xz", "-t"},
		"zst": {"zstd", "-t", "-q"},
		"lz4": {"lz4", "-t", "-q"},
		"br":  {"brotli", "-t"},
		"zip": {"unzip", "-tq"},
	}
	for format, cmd := range tools {
		t.Run(format, func(t *testing.T) {
			if _, err := exec.LookPath(cmd[0]); err != nil {
				t.Skip(cmd[0] + " not installed")
			}
			tmp := t.TempDir()
			p := filepath.Join(tmp, "data.txt")
			must(t, os.WriteFile(p, []byte(strings.Repeat("interop ", 5000)), 0o644))
			outs, err := Compress(ctx, []string{p}, CompressOptions{Format: format, Level: 6}, nil)
			must(t, err)
			out, err := exec.Command(cmd[0], append(cmd[1:], outs[0])...).CombinedOutput()
			if err != nil {
				t.Errorf("%s rejected %s: %v\n%s", cmd[0], outs[0], err, out)
			}
		})
	}
	// tar.gz made by the system tar must extract.
	tmp := t.TempDir()
	src := makeTree(t, tmp)
	tgz := filepath.Join(tmp, "sys.tar.gz")
	if out, err := exec.Command("tar", "-czf", tgz, "-C", tmp, filepath.Base(src)).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v %s", err, out)
	}
	res, err := Extract(ctx, tgz, ExtractOptions{DestDir: filepath.Join(tmp, "x")}, nil)
	must(t, err)
	if _, err := os.Stat(filepath.Join(res, "sub", "日本語.txt")); err != nil {
		t.Error(err)
	}
}

func TestZipLegacyEncoding(t *testing.T) {
	tmp := t.TempDir()
	zp := filepath.Join(tmp, "old.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	// "Grüße.txt" in CP437 (ü = 0x81, ß = 0xE1), no UTF-8 flag.
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "Gr\x81\xe1e.txt", Method: zip.Store, NonUTF8: true})
	w.Write([]byte("hi"))
	zw.Close()
	f.Close()
	res, err := Extract(context.Background(), zp, ExtractOptions{DestDir: filepath.Join(tmp, "x")}, nil)
	must(t, err)
	if filepath.Base(res) != "Grüße.txt" {
		t.Errorf("decoded name %q", filepath.Base(res))
	}
}

func TestDetectByContent(t *testing.T) {
	tmp := t.TempDir()
	p := filepath.Join(tmp, "a.txt")
	must(t, os.WriteFile(p, []byte("x"), 0o644))
	outs, err := Compress(context.Background(), []string{p}, CompressOptions{Format: "zst"}, nil)
	must(t, err)
	renamed := filepath.Join(tmp, "mystery.bin")
	must(t, os.Rename(outs[0], renamed))
	f, err := Detect(renamed)
	must(t, err)
	if f.Name != "zst" {
		t.Errorf("detected %s", f.Name)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]int64{"100m": 100 << 20, "650MB": 650 << 20, "": 0, "1440k": 1440 << 10} {
		got, err := ParseSize(in)
		if err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v", in, got, err)
		}
	}
	if _, err := ParseSize("10"); err == nil {
		t.Error("tiny split size should fail")
	}
}
