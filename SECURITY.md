# Security policy

## Reporting a vulnerability

Please report security problems privately, not in a public issue.

Use GitHub's private reporting form: <https://github.com/Gitguy268/pakket/security/advisories/new>

Include what you found, how to reproduce it (a small crafted archive helps; never send one that holds real private files), and your OS and `7zz` version. You should get a reply within a week. Please give the fix time to ship before you publish details.

## Scope

The most useful reports are about:

- a crafted archive that writes outside the extraction folder (`../` names, absolute paths, symlinks or hard links);
- passwords leaking: into the process list, logs, temporary files or the terminal scrollback;
- encryption that is weaker than the README says (7z AES-256, zip AES-256, dmg AES-256, age);
- a crafted archive that crashes pakket or makes it run something;
- the GitHub Actions workflows in this repository.

pakket hands some formats to 7-Zip, `hdiutil`, `aa`, `xip`, `unar` and `lrzip`. A bug inside those programs belongs with their projects, but tell us if pakket calls them unsafely.

## Supported versions

Only the latest release and the `main` branch are supported.
