# Changelog

## 0.1.0 (2026-11-05)

First release.

- Create 25 formats: 7z, zip, tar with gzip, bzip2, xz, zstd, brotli, lz4, lzip or lzma, the same codecs on single files, dmg, iso, wim, Apple Archive, lrzip and age.
- Extract more than 50 extensions, including rar, cab, msi, exe, deb, rpm, xar/pkg, xip, cpio, cpgz, iso, dmg, wim, vhd/vmdk, squashfs, wpress and, with `unar`, StuffIt and Compact Pro.
- Passwords: AES-256 7z with encrypted names, AES-256 or legacy zip, AES-256 dmg, age passphrases or keys; macOS Keychain storage and lookup.
- Split volumes, one archive per item, delete-after (to the Trash), Mac junk exclusion, solid 7z, checksum and password sidecar files.
- Smart extraction folder, name-clash handling, quarantine propagation, legacy zip name encodings, inner tar unpacking.
- Interactive TUI with a file browser, option forms, a job queue with progress, a contents view and saved defaults.
