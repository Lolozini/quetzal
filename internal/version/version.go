// Package version exposes build information stamped into the binaries at link
// time. Defaults indicate an unstamped (local `go build`) build.
package version

import (
	"fmt"
	"os"
	"runtime"
)

// These are overridden via -ldflags at build time, e.g.:
//
//	-X github.com/lolozini/quetzal/internal/version.Version=1.2.3
var (
	// Version is the release version (a git tag like v1.2.3, or "dev").
	Version = "dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "none"
	// Date is the RFC3339 build timestamp.
	Date = "unknown"
)

// Info is the machine-readable build information.
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	Go      string `json:"go"`
}

// Get returns the current build information.
func Get() Info {
	return Info{Version: Version, Commit: Commit, Date: Date, Go: runtime.Version()}
}

// String renders a one-line, human-readable version banner.
func String() string {
	return fmt.Sprintf("quetzal %s (commit %s, built %s, %s)", Version, Commit, Date, runtime.Version())
}

// HandleFlag prints the version and exits when the program is invoked as
// `<binary> version` (or with --version/-v). Call it at the start of main().
func HandleFlag() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version", "-v":
			fmt.Println(String())
			os.Exit(0)
		}
	}
}
