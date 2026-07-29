package version

import (
	"fmt"
	"runtime"
)

var (
	Version   = "dev"
	GitCommit = "unknown"
	BuildDate = "unknown"
)

// Application metadata shared by --version and the tray About dialog.
const (
	Name    = "VLGR"
	Tagline = "Self-hosted HTTP/TCP/TLS tunnel — expose a local port through a public relay"
	Repo    = "https://github.com/varmtlys/vlgr"
	// The @handle doubles as the profile link (github.com/varmtlys).
	Author  = "Ildar Latypov <varmtlys@gmail.com>"
	License = "MIT © 2026 Ildar Latypov — see LICENSE"
)

func String() string {
	return fmt.Sprintf("%s (commit: %s, built: %s)", Version, GitCommit, BuildDate)
}

// About returns the application info block; bin is the program name
// (vlgr-client / vlgr-server).
func About(bin string) string {
	return fmt.Sprintf(`%s — %s
%s

Version:  %s
Commit:   %s
Built:    %s
Platform: %s/%s (%s)
Repo:     %s
Author:   %s
License:  %s
`, Name, bin, Tagline, Version, GitCommit, BuildDate, runtime.GOOS, runtime.GOARCH, runtime.Version(), Repo, Author, License)
}
