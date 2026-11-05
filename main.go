// pakket is a terminal archiver: compress, extract, encrypt, list and test
// archives in a few dozen formats, from a TUI or from scripts.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Gitguy268/pakket/internal/config"
	"github.com/Gitguy268/pakket/internal/engine"
	"github.com/Gitguy268/pakket/internal/tui"
	"golang.org/x/term"
)

// version is set by the build (-ldflags "-X main.version=...").
var version = "0.1.0"

const usage = `pakket %s: compress, extract and encrypt archives in the terminal

Usage:
  pakket                          open the interactive file browser
  pakket <files...>               extract archives, compress anything else (default format)
  pakket compress [options] <files...>    alias: c, a
  pakket extract  [options] <archives...> alias: x, e
  pakket list     [--json] <archive>      alias: l, ls
  pakket test     <archives...>           alias: t
  pakket encrypt  [options] <files...>    age encryption with a passphrase or keys
  pakket decrypt  [options] <files.age...>
  pakket formats                          what can be created and extracted here
  pakket config   [get <key> | set <key> <value> | reset | path]
  pakket keychain save|delete <archive>   store a password for an archive
  pakket version

Run "pakket <command> -h" for the options of a command.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:]))
}

func run(ctx context.Context, args []string) int {
	cfg := config.Load()
	if len(args) == 0 {
		if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
			fmt.Fprintf(os.Stderr, usage, version)
			return 2
		}
		if err := tui.Run(cfg); err != nil {
			fmt.Fprintln(os.Stderr, "pakket:", err)
			return 1
		}
		return 0
	}
	cmd, rest := args[0], args[1:]
	var err error
	switch cmd {
	case "compress", "c", "a", "add":
		err = cmdCompress(ctx, cfg, rest, "")
	case "extract", "x", "e":
		err = cmdExtract(ctx, cfg, rest)
	case "encrypt", "enc":
		err = cmdCompress(ctx, cfg, rest, "age")
	case "decrypt", "dec":
		err = cmdExtract(ctx, cfg, rest)
	case "list", "l", "ls":
		err = cmdList(ctx, cfg, rest)
	case "test", "t", "verify":
		err = cmdTest(ctx, cfg, rest)
	case "formats":
		cmdFormats()
	case "config":
		err = cmdConfig(cfg, rest)
	case "keychain":
		err = cmdKeychain(rest)
	case "version", "-v", "--version":
		fmt.Println("pakket", version)
	case "help", "-h", "--help":
		fmt.Printf(usage, version)
	default:
		if strings.HasPrefix(cmd, "-") {
			fmt.Fprintf(os.Stderr, usage, version)
			return 2
		}
		err = cmdDrop(ctx, cfg, args)
	}
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "pakket: cancelled")
			return 130
		}
		fmt.Fprintln(os.Stderr, "pakket:", err)
		return 1
	}
	return 0
}

// cmdDrop mirrors dropping files on Keka: archives are extracted and
// everything else is compressed with the default settings.
func cmdDrop(ctx context.Context, cfg config.Config, files []string) error {
	var archives, others []string
	for _, f := range files {
		if _, err := os.Lstat(f); err != nil {
			return err
		}
		if engine.IsArchive(f) {
			archives = append(archives, f)
		} else {
			others = append(others, f)
		}
	}
	if len(archives) > 0 {
		if err := cmdExtract(ctx, cfg, archives); err != nil {
			return err
		}
	}
	if len(others) > 0 {
		return cmdCompress(ctx, cfg, others, "")
	}
	return nil
}

const compressHelp = `Usage: pakket compress [options] <files or folders...>

  -f, --format NAME      7z, zip, tar, tar.gz, tar.bz2, tar.xz, tar.zst, tar.br, tar.lz4,
                         tar.lz, gz, bz2, xz, zst, br, lz4, lz, lzma, dmg, iso, wim, aar,
                         lrz, age (default from config: %s)
  -l, --level 0-9        0 stores only, 9 is maximum compression (default %d)
  -p, --password PASS    encrypt (7z, zip, dmg, age); also read from $PAKKET_PASSWORD
  -P, --ask-password     prompt for the password twice
      --show-names       7z: do not encrypt file names
      --zip-legacy       zip: ZipCrypto instead of AES-256 (opens everywhere, weak)
      --no-solid         7z: non-solid archive (faster to update, larger)
      --keep-mac         keep .DS_Store, ._ files and __MACOSX
  -x, --exclude GLOB     skip names matching GLOB (repeatable)
  -s, --separate         one archive per input
      --delete           move the originals to the Trash afterwards
      --split SIZE       volumes of SIZE (e.g. 100m, 700m, 4.7g, 4095m)
  -o, --output PATH      archive path (single archive only)
  -d, --dest DIR         folder for the archive (default: next to the input)
  -n, --name NAME        archive name without extension
      --volume-name NAME dmg / iso volume name
      --checksum KIND    also write an md5 or sha256 file
      --export-password  write the password to <archive>.txt
      --keychain         save the password in the macOS Keychain
  -r, --recipient KEY    age: public key or file of keys, instead of a passphrase (repeatable)
      --overwrite        replace an existing archive instead of naming it "x 2"
      --threads N        7-Zip threads (default: all cores)
      --notify           show a notification when done
  -q, --quiet            no progress output
`

func cmdCompress(ctx context.Context, cfg config.Config, args []string, forced string) error {
	fs := flag.NewFlagSet("compress", flag.ContinueOnError)
	def := cfg.Format
	if forced != "" {
		def = forced
	}
	fs.Usage = func() { fmt.Fprintf(os.Stderr, compressHelp, cfg.Format, cfg.Level) }
	format := fs.String("format", def, "")
	level := fs.Int("level", cfg.Level, "")
	password := fs.String("password", os.Getenv("PAKKET_PASSWORD"), "")
	ask := fs.Bool("ask-password", false, "")
	showNames := fs.Bool("show-names", !cfg.EncryptNames, "")
	zipLegacy := fs.Bool("zip-legacy", cfg.ZipLegacy, "")
	noSolid := fs.Bool("no-solid", !cfg.Solid, "")
	keepMac := fs.Bool("keep-mac", !cfg.ExcludeMac, "")
	var excludes, recipients multi
	fs.Var(&excludes, "exclude", "")
	separate := fs.Bool("separate", cfg.Separate, "")
	del := fs.Bool("delete", cfg.DeleteAfterPack, "")
	split := fs.String("split", cfg.Split, "")
	output := fs.String("output", "", "")
	dest := fs.String("dest", cfg.CompressDest, "")
	name := fs.String("name", "", "")
	vol := fs.String("volume-name", "", "")
	checksum := fs.String("checksum", cfg.Checksum, "")
	export := fs.Bool("export-password", cfg.ExportPassword, "")
	keychain := fs.Bool("keychain", false, "")
	fs.Var(&recipients, "recipient", "")
	overwrite := fs.Bool("overwrite", false, "")
	threads := fs.Int("threads", 0, "")
	notify := fs.Bool("notify", cfg.Notify, "")
	quiet := fs.Bool("quiet", false, "")
	for s, l := range map[string]string{"f": "format", "l": "level", "p": "password", "P": "ask-password", "x": "exclude",
		"s": "separate", "o": "output", "d": "dest", "n": "name", "r": "recipient", "q": "quiet"} {
		alias(fs, s, l)
	}
	files, err := parseAll(fs, args)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		fs.Usage()
		return errors.New("no files given")
	}
	size, err := engine.ParseSize(*split)
	if err != nil {
		return err
	}
	if *ask {
		pw, err := askPassword(true)
		if err != nil {
			return err
		}
		*password = pw
	}
	if *format == "age" && *password == "" && len(recipients) == 0 {
		pw, err := askPassword(true)
		if err != nil {
			return err
		}
		*password = pw
	}
	o := engine.CompressOptions{
		Format: *format, Level: *level, Password: *password, EncryptNames: !*showNames, ZipLegacy: *zipLegacy,
		Solid: !*noSolid, ExcludeMac: !*keepMac, Exclude: excludes, Separate: *separate, DeleteAfter: *del,
		SplitSize: size, Output: *output, DestDir: *dest, Name: *name, VolumeName: *vol, Checksum: *checksum,
		ExportPassword: *export, AgeRecipients: recipients, Overwrite: *overwrite, Threads: *threads,
	}
	bar := newBar(*quiet)
	start := time.Now()
	outs, err := engine.Compress(ctx, files, o, bar.update)
	bar.done()
	if err != nil {
		if *notify {
			engine.Notify("pakket", "Compression failed: "+err.Error())
		}
		return err
	}
	for _, out := range outs {
		fmt.Println(out)
	}
	if *keychain && o.Password != "" && len(outs) > 0 {
		for _, out := range outs {
			if err := engine.KeychainSave(out, o.Password); err != nil {
				return err
			}
		}
	}
	if !*quiet {
		fmt.Fprintf(os.Stderr, "done in %s, %s\n", time.Since(start).Round(time.Millisecond), ratio(files, outs))
	}
	if *notify {
		engine.Notify("pakket", fmt.Sprintf("Compressed into %s", filepath.Base(outs[0])))
	}
	return nil
}

const extractHelp = `Usage: pakket extract [options] <archives...>

  -d, --dest DIR         where to extract (default: next to the archive)
      --folder MODE      smart: a folder only when the archive has several items
                         always: always a folder; never: straight into DEST (default %s)
  -p, --password PASS    password; also read from $PAKKET_PASSWORD
  -P, --ask-password     prompt for the password
      --no-keychain      do not look up a stored password
      --delete           move the archive (and its volumes) to the Trash afterwards
      --no-quarantine    do not copy the download quarantine flag to the contents
  -e, --encoding NAME    file name encoding of old zips: auto, cp437, shift_jis, gbk,
                         big5, euc-kr, cp866, windows-1251 ... (default %s)
      --overwrite MODE   rename (default), overwrite or skip existing items
      --ignore-gz-name   use the archive name, not the name stored inside a .gz
      --no-nested        leave a .tar found inside a 7z, lrz or age file packed
  -i, --identity FILE    age identity (private key) file (repeatable)
      --notify           show a notification when done
  -q, --quiet            no progress output
`

type extractFlags struct {
	fs       *flag.FlagSet
	o        engine.ExtractOptions
	ask      *bool
	keychain *bool
	notify   *bool
	quiet    *bool
	jsonOut  *bool
	finish   func()
}

func newExtractFlags(name string, cfg config.Config) *extractFlags {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprintf(os.Stderr, extractHelp, cfg.Folder, cfg.Encoding) }
	e := &extractFlags{fs: fs}
	fs.StringVar(&e.o.DestDir, "dest", cfg.ExtractDest, "")
	fs.StringVar(&e.o.Folder, "folder", cfg.Folder, "")
	fs.StringVar(&e.o.Password, "password", os.Getenv("PAKKET_PASSWORD"), "")
	e.ask = fs.Bool("ask-password", false, "")
	noKey := fs.Bool("no-keychain", !cfg.Keychain, "")
	fs.BoolVar(&e.o.DeleteAfter, "delete", cfg.DeleteAfterX, "")
	noQ := fs.Bool("no-quarantine", !cfg.Quarantine, "")
	fs.StringVar(&e.o.Encoding, "encoding", cfg.Encoding, "")
	fs.StringVar(&e.o.Overwrite, "overwrite", cfg.Overwrite, "")
	fs.BoolVar(&e.o.IgnoreGzName, "ignore-gz-name", cfg.IgnoreGzName, "")
	noNested := fs.Bool("no-nested", !cfg.Nested, "")
	var ids multi
	fs.Var(&ids, "identity", "")
	e.notify = fs.Bool("notify", cfg.Notify, "")
	e.quiet = fs.Bool("quiet", false, "")
	e.jsonOut = fs.Bool("json", false, "")
	for s, l := range map[string]string{"d": "dest", "p": "password", "P": "ask-password", "e": "encoding", "i": "identity", "q": "quiet"} {
		alias(fs, s, l)
	}
	e.keychain = new(bool)
	e.finish = func() {
		*e.keychain = !*noKey
		e.o.Quarantine = !*noQ
		e.o.Nested = !*noNested
		e.o.Identities = ids
	}
	return e
}

func (e *extractFlags) parse(args []string) ([]string, error) {
	files, err := parseAll(e.fs, args)
	if err != nil {
		return nil, err
	}
	e.finish()
	if *e.ask {
		pw, err := askPassword(false)
		if err != nil {
			return nil, err
		}
		e.o.Password = pw
	}
	if len(files) == 0 {
		e.fs.Usage()
		return nil, errors.New("no archives given")
	}
	return files, nil
}

// withPassword runs fn, adding a Keychain password and then prompting
// (up to three times) when the archive turns out to be encrypted.
func withPassword(archive string, e *extractFlags, fn func(o engine.ExtractOptions) error) error {
	o := e.o
	if *e.keychain {
		if pw, ok := engine.KeychainFind(archive); ok {
			o.Passwords = append(o.Passwords, pw)
		}
	}
	err := fn(o)
	for tries := 0; errors.Is(err, engine.ErrPassword) && tries < 3 && term.IsTerminal(int(os.Stdin.Fd())); tries++ {
		fmt.Fprintf(os.Stderr, "%s is encrypted.\n", filepath.Base(archive))
		pw, perr := askPassword(false)
		if perr != nil {
			return perr
		}
		o.Password, o.Passwords = pw, nil
		err = fn(o)
	}
	return err
}

func cmdExtract(ctx context.Context, cfg config.Config, args []string) error {
	e := newExtractFlags("extract", cfg)
	files, err := e.parse(args)
	if err != nil {
		return err
	}
	var failed []string
	for _, a := range dedupeParts(files) {
		bar := newBar(*e.quiet)
		var res string
		err := withPassword(a, e, func(o engine.ExtractOptions) error {
			var err error
			res, err = engine.Extract(ctx, a, o, bar.update)
			return err
		})
		bar.done()
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}
			fmt.Fprintf(os.Stderr, "pakket: %s: %v\n", filepath.Base(a), err)
			failed = append(failed, a)
			continue
		}
		fmt.Println(res)
	}
	if *e.notify {
		if len(failed) > 0 {
			engine.Notify("pakket", fmt.Sprintf("%d archive(s) could not be extracted", len(failed)))
		} else {
			engine.Notify("pakket", "Extraction finished")
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d archives failed", len(failed), len(dedupeParts(files)))
	}
	return nil
}

// dedupeParts keeps one name per split archive (x.7z.001, x.7z.002 -> x.7z.001).
func dedupeParts(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		k := engine.Parts(f)[0]
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

func cmdList(ctx context.Context, cfg config.Config, args []string) error {
	e := newExtractFlags("list", cfg)
	e.fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: pakket list [--json] [-p PASS | -P] [-e ENCODING] <archive>")
	}
	files, err := e.parse(args)
	if err != nil {
		return err
	}
	for i, a := range dedupeParts(files) {
		var es []engine.Entry
		err := withPassword(a, e, func(o engine.ExtractOptions) error {
			var err error
			es, err = engine.List(ctx, a, o)
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(a), err)
		}
		if *e.jsonOut {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(es); err != nil {
				return err
			}
			continue
		}
		if len(files) > 1 {
			if i > 0 {
				fmt.Println()
			}
			fmt.Println(a + ":")
		}
		printEntries(os.Stdout, es)
	}
	return nil
}

func printEntries(w io.Writer, es []engine.Entry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', tabwriter.AlignRight)
	var total int64
	files := 0
	fmt.Fprintln(tw, "Size\tPacked\tModified\t \tName")
	for _, e := range es {
		size, packed, mod := "-", "-", ""
		if e.Dir {
			size = "dir"
		} else {
			files++
			if e.Size >= 0 {
				size = human(e.Size)
				total += e.Size
			}
		}
		if e.Packed >= 0 && !e.Dir {
			packed = human(e.Packed)
		}
		if !e.Modified.IsZero() {
			mod = e.Modified.Local().Format("2006-01-02 15:04")
		}
		lock := " "
		if e.Encrypted {
			lock = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", size, packed, mod, lock, e.Name)
	}
	tw.Flush()
	fmt.Fprintf(w, "%d files, %s\n", files, human(total))
}

func cmdTest(ctx context.Context, cfg config.Config, args []string) error {
	e := newExtractFlags("test", cfg)
	e.fs.Usage = func() { fmt.Fprintln(os.Stderr, "Usage: pakket test [-p PASS | -P] <archives...>") }
	files, err := e.parse(args)
	if err != nil {
		return err
	}
	bad := 0
	for _, a := range dedupeParts(files) {
		bar := newBar(*e.quiet)
		err := withPassword(a, e, func(o engine.ExtractOptions) error {
			return engine.Test(ctx, a, o, bar.update)
		})
		bar.done()
		if err != nil {
			bad++
			fmt.Printf("FAIL  %s: %v\n", a, err)
		} else {
			fmt.Printf("OK    %s\n", a)
		}
	}
	if bad > 0 {
		return fmt.Errorf("%d archive(s) failed the test", bad)
	}
	return nil
}

func cmdFormats() {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FORMAT\tCREATE\tEXTRACT\tPASSWORD\tEXTENSIONS\tNOTES")
	for _, f := range engine.Formats {
		create, extract := "-", "yes"
		if f.Create {
			create = "yes"
		}
		note := f.Note
		if f.Needs != "" {
			if _, ok := engine.Tool(f.Needs); !ok {
				if f.Create {
					create = "missing"
				}
				extract = "missing"
				note = strings.TrimPrefix(note+"; needs "+f.Needs, "; ")
			}
		}
		pw := ""
		if f.Encrypt {
			pw = "yes"
		}
		exts := f.Exts
		if len(exts) > 4 {
			exts = append(exts[:4:4], "…")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", f.Name, create, extract, pw, strings.Join(exts, " "), note)
	}
	tw.Flush()
	fmt.Println("\nFormats not listed may still open: 7-Zip is tried on unknown files.")
}

func cmdConfig(cfg config.Config, args []string) error {
	if len(args) == 0 {
		b, _ := json.MarshalIndent(cfg, "", "  ")
		fmt.Println(string(b))
		return nil
	}
	switch args[0] {
	case "path":
		fmt.Println(config.Path())
	case "reset":
		return config.Save(config.Default())
	case "get":
		if len(args) != 2 {
			return errors.New("usage: pakket config get <key>")
		}
		v, err := config.Get(cfg, args[1])
		if err != nil {
			return err
		}
		fmt.Println(v)
	case "set":
		if len(args) != 3 {
			return errors.New("usage: pakket config set <key> <value>")
		}
		if err := config.Set(&cfg, args[1], args[2]); err != nil {
			return err
		}
		return config.Save(cfg)
	case "keys":
		fmt.Println(strings.Join(config.Keys(), "\n"))
	default:
		return fmt.Errorf("unknown config command %q (get, set, reset, path, keys)", args[0])
	}
	return nil
}

func cmdKeychain(args []string) error {
	if len(args) != 2 {
		return errors.New("usage: pakket keychain save|delete <archive>")
	}
	switch args[0] {
	case "save":
		pw, err := askPassword(false)
		if err != nil {
			return err
		}
		return engine.KeychainSave(args[1], pw)
	case "delete":
		return engine.KeychainDelete(args[1])
	}
	return fmt.Errorf("unknown keychain command %q", args[0])
}

func askPassword(confirm bool) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("a password is needed; pass -p or set PAKKET_PASSWORD")
	}
	defer tty.Close()
	read := func(prompt string) (string, error) {
		fmt.Fprint(tty, prompt)
		b, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		return string(b), err
	}
	pw, err := read("Password: ")
	if err != nil {
		return "", err
	}
	if pw == "" {
		return "", errors.New("empty password")
	}
	if confirm {
		again, err := read("Repeat:   ")
		if err != nil {
			return "", err
		}
		if again != pw {
			return "", errors.New("the passwords do not match")
		}
	}
	return pw, nil
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

func ratio(inputs, outputs []string) string {
	var in, out int64
	for _, p := range inputs {
		_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
			if err == nil && info.Mode().IsRegular() {
				in += info.Size()
			}
			return nil
		})
	}
	for _, p := range outputs {
		if st, err := os.Stat(p); err == nil {
			out += st.Size()
		}
	}
	if in == 0 {
		return human(out)
	}
	return fmt.Sprintf("%s -> %s (%.0f%%)", human(in), human(out), float64(out)*100/float64(in))
}
