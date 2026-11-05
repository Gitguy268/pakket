// Package engine creates, extracts, lists and tests archives.
package engine

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind says which code path handles a format.
type Kind int

const (
	KindTar    Kind = iota // tar, optionally wrapped in a stream codec
	KindStream             // a single compressed file
	KindZip                // zip and zip-based containers
	Kind7z                 // handled by the 7zz binary
	KindDMG                // hdiutil
	KindISO                // hdiutil makehybrid / 7zz
	KindAAR                // Apple Archive, /usr/bin/aa
	KindAge                // age encryption
	KindLrzip              // lrzip binary
	KindXip                // /usr/bin/xip
	KindWpress             // All-in-One WP Migration
	KindUnar               // unar binary (old Mac formats)
)

// Format describes one archive type.
type Format struct {
	Name    string   // canonical name, used with --format
	Exts    []string // extensions, first is the one used when creating
	Kind    Kind
	Codec   string // stream codec for KindTar / KindStream ("" = plain tar)
	Create  bool   // pakket can create it
	Encrypt bool   // supports a password when creating
	Needs   string // external tool, if any
	Note    string
}

// Formats is the full table. Order matters for display.
var Formats = []Format{
	{Name: "7z", Exts: []string{".7z"}, Kind: Kind7z, Create: true, Encrypt: true, Needs: "7zz", Note: "AES-256, encrypted file names, solid"},
	{Name: "zip", Exts: []string{".zip"}, Kind: KindZip, Create: true, Encrypt: true, Note: "AES-256 or legacy ZipCrypto (via 7zz)"},
	{Name: "tar", Exts: []string{".tar"}, Kind: KindTar, Create: true},
	{Name: "tar.gz", Exts: []string{".tar.gz", ".tgz"}, Kind: KindTar, Codec: "gzip", Create: true},
	{Name: "tar.bz2", Exts: []string{".tar.bz2", ".tbz2", ".tbz"}, Kind: KindTar, Codec: "bzip2", Create: true},
	{Name: "tar.xz", Exts: []string{".tar.xz", ".txz"}, Kind: KindTar, Codec: "xz", Create: true},
	{Name: "tar.zst", Exts: []string{".tar.zst", ".tzst"}, Kind: KindTar, Codec: "zstd", Create: true},
	{Name: "tar.br", Exts: []string{".tar.br"}, Kind: KindTar, Codec: "brotli", Create: true},
	{Name: "tar.lz4", Exts: []string{".tar.lz4"}, Kind: KindTar, Codec: "lz4", Create: true},
	{Name: "tar.lz", Exts: []string{".tar.lz", ".tlz"}, Kind: KindTar, Codec: "lzip", Create: true},
	{Name: "tar.lzma", Exts: []string{".tar.lzma"}, Kind: KindTar, Codec: "lzma", Create: true},
	{Name: "gz", Exts: []string{".gz", ".gzip"}, Kind: KindStream, Codec: "gzip", Create: true, Note: "folders become .tar.gz"},
	{Name: "bz2", Exts: []string{".bz2", ".bzip2"}, Kind: KindStream, Codec: "bzip2", Create: true, Note: "folders become .tar.bz2"},
	{Name: "xz", Exts: []string{".xz"}, Kind: KindStream, Codec: "xz", Create: true, Note: "folders become .tar.xz"},
	{Name: "zst", Exts: []string{".zst", ".zstd"}, Kind: KindStream, Codec: "zstd", Create: true, Note: "folders become .tar.zst"},
	{Name: "br", Exts: []string{".br"}, Kind: KindStream, Codec: "brotli", Create: true, Note: "folders become .tar.br"},
	{Name: "lz4", Exts: []string{".lz4"}, Kind: KindStream, Codec: "lz4", Create: true, Note: "folders become .tar.lz4"},
	{Name: "lz", Exts: []string{".lz", ".lzip"}, Kind: KindStream, Codec: "lzip", Create: true, Note: "folders become .tar.lz"},
	{Name: "lzma", Exts: []string{".lzma"}, Kind: KindStream, Codec: "lzma", Create: true, Note: "folders become .tar.lzma"},
	{Name: "dmg", Exts: []string{".dmg"}, Kind: KindDMG, Create: true, Encrypt: true, Needs: "hdiutil", Note: "AES-256 encrypted disk image, macOS"},
	{Name: "iso", Exts: []string{".iso"}, Kind: KindISO, Create: true, Needs: "hdiutil", Note: "UDF + ISO9660 + Joliet, macOS"},
	{Name: "wim", Exts: []string{".wim", ".swm", ".esd"}, Kind: Kind7z, Create: true, Needs: "7zz", Note: "Windows Imaging"},
	{Name: "aar", Exts: []string{".aar", ".yaa"}, Kind: KindAAR, Create: true, Needs: "aa", Note: "Apple Archive (LZFSE), macOS"},
	{Name: "lrz", Exts: []string{".lrz"}, Kind: KindLrzip, Create: true, Needs: "lrzip", Note: "folders become .tar.lrz"},
	{Name: "age", Exts: []string{".age"}, Kind: KindAge, Create: true, Encrypt: true, Note: "encrypt any file or folder with a passphrase or age keys"},

	// Extract only.
	{Name: "rar", Exts: []string{".rar", ".cbr"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "zipx", Exts: []string{".zipx"}, Kind: KindZip},
	{Name: "jar", Exts: []string{".jar", ".war", ".ear", ".ipa", ".apk", ".xpi", ".appx", ".msix", ".cbz", ".epub", ".whl", ".nupkg", ".vsix", ".aab"}, Kind: KindZip},
	{Name: "cab", Exts: []string{".cab"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "msi", Exts: []string{".msi", ".msp"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "exe", Exts: []string{".exe"}, Kind: Kind7z, Needs: "7zz", Note: "self-extracting and installer archives"},
	{Name: "cpio", Exts: []string{".cpio"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "cpgz", Exts: []string{".cpgz"}, Kind: KindStream, Codec: "gzip", Note: "gzip-wrapped cpio"},
	{Name: "pax", Exts: []string{".pax"}, Kind: KindTar},
	{Name: "pax.gz", Exts: []string{".pax.gz"}, Kind: KindTar, Codec: "gzip"},
	{Name: "spk", Exts: []string{".spk"}, Kind: KindTar, Note: "Synology package"},
	{Name: "xar", Exts: []string{".xar", ".pkg", ".mpkg"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "xip", Exts: []string{".xip"}, Kind: KindXip, Needs: "xip"},
	{Name: "deb", Exts: []string{".deb", ".udeb"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "rpm", Exts: []string{".rpm"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "Z", Exts: []string{".z", ".taz"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "arj", Exts: []string{".arj"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "lzh", Exts: []string{".lzh", ".lha"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "chm", Exts: []string{".chm"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "vhd", Exts: []string{".vhd", ".vhdx", ".vmdk", ".vdi", ".qcow2", ".img"}, Kind: Kind7z, Needs: "7zz", Note: "disk images"},
	{Name: "squashfs", Exts: []string{".squashfs", ".sfs", ".snap", ".appimage"}, Kind: Kind7z, Needs: "7zz"},
	{Name: "wpress", Exts: []string{".wpress"}, Kind: KindWpress, Note: "All-in-One WP Migration backup"},
	{Name: "cpt", Exts: []string{".cpt"}, Kind: KindUnar, Needs: "unar", Note: "Compact Pro"},
	{Name: "sit", Exts: []string{".sit", ".sitx", ".sea"}, Kind: KindUnar, Needs: "unar", Note: "StuffIt"},
	{Name: "is3", Exts: []string{".is3"}, Kind: KindUnar, Needs: "unar", Note: "InstallShield 3"},
}

// ByName finds a format by its name or one of its extensions.
func ByName(name string) (Format, bool) {
	n := strings.ToLower(strings.TrimPrefix(name, "."))
	for _, f := range Formats {
		if strings.ToLower(f.Name) == n {
			return f, true
		}
		for _, e := range f.Exts {
			if e[1:] == n {
				return f, true
			}
		}
	}
	return Format{}, false
}

// CreateNames lists the formats pakket can create.
func CreateNames() []string {
	var out []string
	for _, f := range Formats {
		if f.Create {
			out = append(out, f.Name)
		}
	}
	return out
}

// byExt matches the longest known extension of a file name.
func byExt(name string) (Format, bool) {
	low := strings.ToLower(name)
	type cand struct {
		f   Format
		ext string
	}
	var cs []cand
	for _, f := range Formats {
		for _, e := range f.Exts {
			if strings.HasSuffix(low, e) {
				cs = append(cs, cand{f, e})
			}
		}
	}
	if len(cs) == 0 {
		return Format{}, false
	}
	sort.SliceStable(cs, func(i, j int) bool { return len(cs[i].ext) > len(cs[j].ext) })
	return cs[0].f, true
}

// StripExt removes a known archive extension (and split suffix) from a name.
func StripExt(name string) string {
	name = stripSplit(name)
	low := strings.ToLower(name)
	best := ""
	for _, f := range Formats {
		for _, e := range f.Exts {
			if strings.HasSuffix(low, e) && len(e) > len(best) {
				best = e
			}
		}
	}
	if best != "" && len(name) > len(best) {
		return name[:len(name)-len(best)]
	}
	return name
}

func stripSplit(name string) string {
	if p, ok := splitBase(name); ok {
		return p
	}
	return name
}

// splitBase returns "x.zip" for "x.zip.001" style names.
func splitBase(name string) (string, bool) {
	ext := filepath.Ext(name)
	if len(ext) == 4 && isDigits(ext[1:]) {
		return strings.TrimSuffix(name, ext), true
	}
	return "", false
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

var (
	magic7z    = []byte{'7', 'z', 0xBC, 0xAF, 0x27, 0x1C}
	magicRar   = []byte("Rar!\x1a\x07")
	magicGzip  = []byte{0x1f, 0x8b}
	magicBz2   = []byte("BZh")
	magicXz    = []byte{0xFD, '7', 'z', 'X', 'Z', 0x00}
	magicZstd  = []byte{0x28, 0xB5, 0x2F, 0xFD}
	magicLz4   = []byte{0x04, 0x22, 0x4D, 0x18}
	magicLzip  = []byte("LZIP")
	magicXar   = []byte("xar!")
	magicCab   = []byte("MSCF")
	magicOLE   = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}
	magicAge   = []byte("age-encryption.org/v1")
	magicAgeA  = []byte("-----BEGIN AGE ENCRYPTED FILE-----")
	magicLrz   = []byte("LRZI")
	magicCpio  = []byte("0707")
	magicDeb   = []byte("!<arch>\ndebian")
	magicRpm   = []byte{0xED, 0xAB, 0xEE, 0xDB}
	magicZ     = []byte{0x1F, 0x9D}
	magicSqfs  = []byte("hsqs")
	magicAA    = [][]byte{[]byte("AA01"), []byte("YAA1"), []byte("pbze"), []byte("pbzx"), []byte("pbzz"), []byte("pbz4"), []byte("pbzb")}
	magicStuff = [][]byte{[]byte("SIT!"), []byte("StuffIt "), []byte("StuffIt!")}
)

// sniff identifies a format from leading bytes. It returns ok=false when unsure.
func sniff(head []byte) (Format, bool) {
	get := ByName
	switch {
	case bytes.HasPrefix(head, magic7z):
		return get("7z")
	case bytes.HasPrefix(head, magicRar):
		return get("rar")
	case bytes.HasPrefix(head, []byte("PK\x03\x04")), bytes.HasPrefix(head, []byte("PK\x05\x06")), bytes.HasPrefix(head, []byte("PK\x07\x08")):
		return get("zip")
	case bytes.HasPrefix(head, magicXz):
		return get("xz")
	case bytes.HasPrefix(head, magicZstd):
		return get("zst")
	case bytes.HasPrefix(head, magicLz4):
		return get("lz4")
	case bytes.HasPrefix(head, magicLzip):
		return get("lz")
	case bytes.HasPrefix(head, magicGzip):
		return get("gz")
	case bytes.HasPrefix(head, magicBz2) && len(head) > 3 && head[3] >= '1' && head[3] <= '9':
		return get("bz2")
	case bytes.HasPrefix(head, magicXar):
		return get("xar")
	case bytes.HasPrefix(head, magicCab):
		return get("cab")
	case bytes.HasPrefix(head, magicOLE):
		return get("msi")
	case bytes.HasPrefix(head, magicAge), bytes.HasPrefix(head, magicAgeA):
		return get("age")
	case bytes.HasPrefix(head, magicLrz):
		return get("lrz")
	case bytes.HasPrefix(head, magicCpio), len(head) > 1 && head[0] == 0xC7 && head[1] == 0x71, len(head) > 1 && head[0] == 0x71 && head[1] == 0xC7:
		return get("cpio")
	case bytes.HasPrefix(head, magicDeb):
		return get("deb")
	case bytes.HasPrefix(head, magicRpm):
		return get("rpm")
	case bytes.HasPrefix(head, magicZ):
		return get("Z")
	case bytes.HasPrefix(head, magicSqfs):
		return get("squashfs")
	case bytes.HasPrefix(head, []byte("MZ")):
		return get("exe")
	case len(head) >= 262 && bytes.Equal(head[257:262], []byte("ustar")):
		return get("tar")
	case len(head) >= 0x8006 && bytes.Equal(head[0x8001:0x8006], []byte("CD001")):
		return get("iso")
	}
	for _, m := range magicAA {
		if bytes.HasPrefix(head, m) {
			return get("aar")
		}
	}
	for _, m := range magicStuff {
		if bytes.HasPrefix(head, m) {
			return get("sit")
		}
	}
	return Format{}, false
}

// Detect works out the format of a file from its contents, then its name.
// Containers that share a magic number (jar, ipa, cpgz, tar.gz ...) are
// refined by extension.
func Detect(path string) (Format, error) {
	f, err := os.Open(path)
	if err != nil {
		return Format{}, err
	}
	defer f.Close()
	head := make([]byte, 0x8010)
	n, _ := io.ReadFull(f, head)
	head = head[:n]

	byName, nameOK := byExt(stripSplit(filepath.Base(path)))
	sn, sniffOK := sniff(head)

	if !sniffOK {
		if isDMG(f) {
			d, _ := ByName("dmg")
			return d, nil
		}
		if nameOK {
			return byName, nil
		}
		return Format{}, errUnknown(path)
	}
	if nameOK {
		// Same family: keep the more specific name-based format.
		if byName.Kind == sn.Kind || (sn.Name == "gz" && byName.Codec == "gzip") ||
			(sn.Name == "xz" && byName.Codec == "xz") || (sn.Name == "bz2" && byName.Codec == "bzip2") ||
			(sn.Name == "zst" && byName.Codec == "zstd") || (sn.Name == "lz4" && byName.Codec == "lz4") ||
			(sn.Name == "lz" && byName.Codec == "lzip") || (sn.Name == "zip" && byName.Kind == KindZip) ||
			(sn.Name == "xar" && byName.Kind == KindXip) || (sn.Name == "msi" && byName.Kind == Kind7z) ||
			(sn.Name == "exe" && byName.Kind != KindTar) || (sn.Name == "zip" && byName.Kind == Kind7z) ||
			(sn.Name == "tar" && byName.Kind == KindTar) {
			return byName, nil
		}
	}
	return sn, nil
}

// isDMG checks for the UDIF "koly" trailer.
func isDMG(f *os.File) bool {
	st, err := f.Stat()
	if err != nil || st.Size() < 512 {
		return false
	}
	b := make([]byte, 4)
	if _, err := f.ReadAt(b, st.Size()-512); err != nil {
		return false
	}
	return string(b) == "koly"
}

// IsArchive reports whether pakket would extract this path.
func IsArchive(path string) bool {
	st, err := os.Stat(path)
	if err != nil || st.IsDir() {
		return false
	}
	if _, ok := byExt(stripSplit(filepath.Base(path))); ok {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := io.ReadFull(f, head)
	sn, ok := sniff(head[:n])
	return ok && sn.Name != "exe"
}
