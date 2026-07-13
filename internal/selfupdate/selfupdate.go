// Package selfupdate keeps a running vlgr binary current with the latest
// GitHub release. It is shared by the client and the server: both poll the
// releases API, and when a newer tag ships they download the matching binary,
// swap it on disk and relaunch. Relaunch is as seamless as the setup allows —
// listeners are closed to free their ports before the replacement starts, and
// vlgr clients reconnect automatically, so an open tunnel is only briefly
// interrupted.
package selfupdate

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// SigningPublicKey is the hex-encoded ed25519 public key that release binaries
// are signed with. It is injected at build time via
// -ldflags "-X vlgr/internal/selfupdate.SigningPublicKey=<hex>". When empty,
// self-update refuses to install anything (fail-safe): there is no trust
// anchor to verify the downloaded binary against.
var SigningPublicKey string

// Config drives the update loop.
type Config struct {
	Repo    string        // GitHub "owner/name", e.g. "varmtlys/vlgr"
	Asset   string        // base asset name, e.g. "vlgr-server" or "vlgr-client"
	Current string        // running version, e.g. version.Version
	Every   time.Duration // poll interval

	// PublicKeyHex overrides SigningPublicKey (hex ed25519) for signature
	// verification. Empty falls back to the compiled-in SigningPublicKey.
	PublicKeyHex string

	// BeforeRestart, when set, runs just before the replacement is launched.
	// Use it to close listeners so the new process can bind their ports.
	BeforeRestart func()
}

// Run polls until ctx is cancelled. A successful update never returns — the
// process is replaced. "dev" builds are skipped: they have no release to match.
func Run(ctx context.Context, cfg Config) {
	if cfg.Current == "dev" || cfg.Current == "" {
		log.Printf("[update] disabled for development build %q", cfg.Current)
		return
	}
	if cfg.Every <= 0 {
		cfg.Every = time.Hour
	}
	ticker := time.NewTicker(cfg.Every)
	defer ticker.Stop()

	for {
		if err := checkOnce(cfg); err != nil {
			log.Printf("[update] check failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func checkOnce(cfg Config) error {
	latest, err := latestTag(cfg.Repo)
	if err != nil {
		return err
	}
	if !newer(cfg.Current, latest) {
		return nil
	}
	log.Printf("[update] newer release %s available (running %s), updating", latest, cfg.Current)
	return apply(cfg, latest)
}

// latestTag returns the tag_name of the latest GitHub release.
func latestTag(repo string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("releases API status %d", resp.StatusCode)
	}
	var body struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", err
	}
	if body.TagName == "" {
		return "", fmt.Errorf("no tag_name in latest release")
	}
	return body.TagName, nil
}

// assetName maps the running platform to the release asset naming used by the
// build scripts: vlgr-<role>-<os>-<arch>[.exe], with GOARCH 386 published as
// "x86".
func assetName(base string) string {
	arch := runtime.GOARCH
	if arch == "386" {
		arch = "x86"
	}
	name := fmt.Sprintf("%s-%s-%s", base, runtime.GOOS, arch)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

// apply downloads the release binary, swaps it in place of the running
// executable and relaunches. It does not return on success.
func apply(cfg Config, tag string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", cfg.Repo, tag, assetName(cfg.Asset))
	newPath := exe + ".new"
	if err := download(url, newPath, exe); err != nil {
		return err
	}

	// Verify the downloaded binary against the detached signature before it is
	// ever executed. A failure here (including no configured key) aborts the
	// update and leaves the running binary untouched.
	if err := verifyDownload(cfg, newPath, url+".sig"); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("signature verification failed: %w", err)
	}

	// Replace the running binary. Windows cannot overwrite a running exe, but
	// it can rename it, so move the old one aside first.
	backup := exe + ".old"
	os.Remove(backup)
	if err := os.Rename(exe, backup); err != nil {
		os.Remove(newPath)
		return fmt.Errorf("move current binary aside: %w", err)
	}
	if err := os.Rename(newPath, exe); err != nil {
		os.Rename(backup, exe) // roll back
		return fmt.Errorf("install new binary: %w", err)
	}

	log.Printf("[update] installed %s, relaunching", tag)
	relaunch(exe, cfg.BeforeRestart)
	return nil // unreachable
}

// download fetches url into dst, copying the source file's permission bits.
func download(url, dst, permFrom string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", url, resp.StatusCode)
	}

	mode := os.FileMode(0755)
	if fi, err := os.Stat(permFrom); err == nil {
		mode = fi.Mode()
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(dst)
		return err
	}
	return f.Close()
}

// verifyDownload checks the binary at path against a detached ed25519
// signature fetched from sigURL, using the configured public key. With no key
// it returns an error so nothing unverified is ever installed.
func verifyDownload(cfg Config, path, sigURL string) error {
	pubHex := cfg.PublicKeyHex
	if pubHex == "" {
		pubHex = SigningPublicKey
	}
	if pubHex == "" {
		return fmt.Errorf("no signing public key configured; refusing unverified binary")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	sig, err := fetchSignature(sigURL)
	if err != nil {
		return err
	}
	return verify(pubHex, data, sig)
}

// verify reports whether sig is a valid ed25519 signature of data under the
// hex-encoded public key.
func verify(pubHex string, data, sig []byte) error {
	pub, err := hex.DecodeString(strings.TrimSpace(pubHex))
	if err != nil {
		return fmt.Errorf("decode public key: %w", err)
	}
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("public key length %d, want %d", len(pub), ed25519.PublicKeySize)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature length %d, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(ed25519.PublicKey(pub), data, sig) {
		return fmt.Errorf("signature does not match binary")
	}
	return nil
}

// fetchSignature downloads a detached signature file (hex-encoded 64-byte
// ed25519 signature) and decodes it.
func fetchSignature(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download signature %s: status %d", url, resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return nil, err
	}
	sig, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil {
		return nil, fmt.Errorf("decode signature: %w", err)
	}
	return sig, nil
}

// relaunch frees listeners (via before), starts the replacement with the same
// arguments and exits so the child can bind the freed ports.
func relaunch(exe string, before func()) {
	if before != nil {
		before()
	}
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[update] relaunch failed: %v", err)
		os.Exit(1)
	}
	os.Exit(0)
}

// newer reports whether latest is a strictly higher version than current.
// Both are compared as dot-separated numeric components after a leading "v";
// a non-numeric or unparseable tag falls back to a plain inequality.
func newer(current, latest string) bool {
	cur := strings.Split(strings.TrimPrefix(current, "v"), ".")
	lat := strings.Split(strings.TrimPrefix(latest, "v"), ".")
	for i := 0; i < len(cur) || i < len(lat); i++ {
		c := numAt(cur, i)
		l := numAt(lat, i)
		if c < 0 || l < 0 {
			return latest != current // non-numeric: only update on a change
		}
		if l != c {
			return l > c
		}
	}
	return false
}

func numAt(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, err := strconv.Atoi(parts[i])
	if err != nil {
		return -1
	}
	return n
}
