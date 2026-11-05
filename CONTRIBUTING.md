# Contributing

Thanks for helping with pakket.

By taking part you agree to the [Code of Conduct](CODE_OF_CONDUCT.md).

## Reporting a bug or asking for a feature

Open an [issue](https://github.com/Gitguy268/pakket/issues/new/choose) and use the template. Say what you ran (or which keys you pressed), the archive format and which program made it, your OS, and the output of `pakket version` and `7zz | head -2`.

Report security problems privately: see [SECURITY.md](SECURITY.md).

## Making a change

1. Fork the repository and create a branch from `main`.
2. Build and run it. You need Go 1.27 or newer, and 7-Zip for the 7z, rar and other formats it handles (`brew install sevenzip`, or `apt install 7zip`).

   ```sh
   go build -o pakket .
   ./pakket                # TUI
   ./pakket formats        # what works on this machine
   ```

   Set `XDG_CONFIG_HOME` to a temporary folder to keep a test run away from your own settings.

3. Make your change. Keep commits small, each with a clear message.
4. Run the checks below and update the README if behaviour changed.
5. Open a pull request and fill in the template.

## Checks

```sh
gofmt -l .          # prints nothing
go vet ./...
go test -race ./...
```

The engine tests make, test, list and extract every format, check passwords, split volumes and path-traversal guards, and compare output with the system `gzip`, `xz`, `zstd`, `lz4`, `brotli` and `unzip`. Tests skip a format when its tool is missing; on macOS they also cover dmg, iso and Apple Archive.

The README demo is recorded with [VHS](https://github.com/charmbracelet/vhs): `vhs docs/demo.tape` with a built `pakket` on your `PATH`.

## Layout

| Path | What it does |
|---|---|
| `main.go`, `args.go`, `bar.go` | the command line: subcommands, flags, password prompts, progress bar |
| `internal/engine` | formats, detection, compress, extract, list and test; the only package that touches archives |
| `internal/tui` | the file browser, option forms, job queue and contents view (Bubble Tea) |
| `internal/config` | the settings file (`pakket config`) |

New dependencies need an MIT-compatible licence and a line in NOTICE. Copyleft code cannot be added; calling a separate copyleft program (like 7-Zip) is fine.
