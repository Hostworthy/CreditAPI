// Package integration_test runs the complete server (real wiring, real HTTP)
// against the fake Claude CLI. The test binary re-executes itself as the CLI.
package integration_test

import (
	"os"
	"testing"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
)

func TestMain(m *testing.M) {
	fakeclaude.MaybeRun()
	os.Exit(m.Run())
}
