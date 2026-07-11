package selfupdate

import (
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
