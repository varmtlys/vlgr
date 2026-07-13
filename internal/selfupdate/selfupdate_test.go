package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v1.2.3", "v1.2.4", true},
		{"v1.2.3", "v1.3.0", true},
		{"v1.2.3", "v2.0.0", true},
		{"1.2.3", "1.2.3", false},
		{"v1.2.3", "v1.2.3", false},
		{"v1.2.4", "v1.2.3", false},
		{"v1.2", "v1.2.1", true},
		{"v1.10.0", "v1.9.0", false},
		{"v1.9.0", "v1.10.0", true},
		{"dev", "v1.0.0", true}, // non-numeric current, differs
	}
	for _, c := range cases {
		if got := newer(c.current, c.latest); got != c.want {
			t.Errorf("newer(%q, %q) = %v, want %v", c.current, c.latest, got, c.want)
		}
	}
}

func TestVerify(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pubHex := hex.EncodeToString(pub)
	data := []byte("this is the release binary")
	sig := ed25519.Sign(priv, data)

	if err := verify(pubHex, data, sig); err != nil {
		t.Errorf("valid signature rejected: %v", err)
	}

	// Tampered binary must fail.
	if err := verify(pubHex, append(data, '!'), sig); err == nil {
		t.Error("tampered binary must fail verification")
	}

	// Signature from a different key must fail.
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	if err := verify(pubHex, data, ed25519.Sign(other, data)); err == nil {
		t.Error("signature from wrong key must fail")
	}

	// Malformed inputs.
	if err := verify("zz", data, sig); err == nil {
		t.Error("non-hex public key must error")
	}
	if err := verify(pubHex, data, sig[:10]); err == nil {
		t.Error("short signature must error")
	}
}

func TestVerifyDownload_NoKeyRefuses(t *testing.T) {
	// With no compiled-in key and no override, updates must be refused.
	if err := verifyDownload(Config{}, "irrelevant", "http://example/sig"); err == nil {
		t.Error("missing signing key must refuse the update")
	}
}

func TestAssetName(t *testing.T) {
	name := assetName("vlgr-server")
	if !strings.HasPrefix(name, "vlgr-server-"+runtime.GOOS+"-") {
		t.Errorf("asset name %q missing os prefix", name)
	}
	if runtime.GOOS == "windows" && !strings.HasSuffix(name, ".exe") {
		t.Errorf("windows asset %q must end in .exe", name)
	}
	if runtime.GOARCH == "386" && !strings.Contains(name, "-x86") {
		t.Errorf("386 must map to x86, got %q", name)
	}
}
