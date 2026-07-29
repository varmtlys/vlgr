## Features

- **`--version` now prints an application info block** — name, tagline, version, commit, build date, platform, repository, author and license instead of a single version line; `vlgr-server` and `vlgr-client` share one source (`version.About`).
- **`About VLGR…` item in the tray menu** — shows the same application info in a native dialog (`Microsoft.VisualBasic` message box on Windows, zenity or kdialog on Linux).

## Fixes

- **`Open in browser` no longer builds a malformed URL for raw TCP/TLS forwards** — their public address already carries a `tcp://` or `tls://` scheme, so the tray produced `http://tcp://host:port`; the item is now disabled for tunnels a browser cannot open (`tray.go`).
- **Tray click watchers are released on shutdown** — `Tray.Stop()` never closed `stopCh`, so the per-slot watcher goroutines stayed blocked for the life of the process.

## Docs

- The project is now released under the **MIT license**: a `LICENSE` file with the copyright notice of Ildar Latypov (@varmtlys), a `License` section in `README.md`, and the same notice in `--version` and the tray About dialog.

**Full Changelog**: https://github.com/varmtlys/vlgr/compare/v2.2...v2.3
