// Package fakes lets a test binary stand in for both the Claude and the
// Cursor CLI. Test packages that start the server call MaybeRun first thing
// in TestMain; the server runs the test binary through symlinks named
// `claude` and `cursor-agent` (see support.NewServer), and the name it was
// started under picks the fake.
package fakes

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
)

// MaybeRun runs the matching fake and exits when this process was started
// as a fake CLI; otherwise it returns.
func MaybeRun() {
	name := strings.ToLower(filepath.Base(os.Args[0]))
	if strings.Contains(name, "cursor") && os.Getenv(fakecursor.EnvMode) != "" {
		os.Exit(fakecursor.Main(os.Args[1:]))
	}
	if os.Getenv(fakeclaude.EnvScenario) != "" {
		os.Exit(fakeclaude.Main(os.Args[1:]))
	}
}
