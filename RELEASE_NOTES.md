Hardening release: the traffic inspector and admin API are locked down to loopback with DNS-rebinding and cross-site protection, self-update downloads are bounded and version comparison is hardened, and security headers now cover streamed responses. Wire format is unchanged — v2.1 clients and servers remain compatible.

## Security

- **Traffic inspector is loopback-only** — the client inspector now verifies the peer is loopback, checks `Host` against DNS-rebinding, and rejects cross-site state changes on replay via an `Origin` check (`dashboard.go`).
- **Admin API trust model tightened** — loopback callers are trusted by `Host`, remote callers must present the Bearer token, and the token path is refused entirely when no token is configured, closing a previously open bind (`admin.go`).
- **Tokenless `--ssh` now warns at startup** — running SSH tunnels with `NoClientAuth` lets anyone open public forwards, so the server logs a warning when `--ssh` runs without a token.
- **Self-update download is bounded** — the release binary is read through `io.LimitReader` so a compromised or misbehaving host cannot fill the disk before the signature is checked.
- **Version comparison hardened** — an unparseable latest tag is treated as not newer (no accidental sidegrade), while a real numeric release still updates a dev build; pre-release suffixes now compare by the leading integer of each component.
- **Security headers apply to streamed responses** — `X-Content-Type-Options` and `X-Frame-Options` defaults are now set on large streamed bodies too, via a shared `setSecurityDefaults` (`proxy.go`).

## Fixes

- **Raw TCP/TLS relays ignore stray HTTP response frames** — `handleHTTPRes` is guarded against a nil response channel, so a bogus `MsgHTTPRes` can no longer block the read loop on a nil send (`handler.go`).
- **`build.ps1` host signing fixed** — `GOOS`/`GOARCH`/`CGO_ENABLED` are cleared before deriving the signing public key (and again on exit), fixing "executable file not found" when the signing helper cross-compiled to the last build target.

## Improvements

- Unused `version.Short()` helper and its test removed (YAGNI).
- Release builds are now produced, signed and published automatically by CI on a version tag; `build.sh` gained the same signed-build support as `build.ps1`.

## Docs

- Documented the inspector and admin access model: inspector is loopback-only with DNS-rebinding and cross-site-replay protection; admin API trusts loopback and requires the Bearer token for remote callers, refused with no token set; tokenless `--ssh` accepts anyone and warns at startup (`README.md`, `GETTING_STARTED.md`).

**Full Changelog**: https://github.com/varmtlys/vlgr/compare/v2.1...v2.2
