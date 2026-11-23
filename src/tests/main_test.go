// Package integration_test runs the complete server (real wiring, real HTTP)
// against the fake Claude and Cursor CLIs. The test binary re-executes itself
// as either CLI.
package integration_test

import (
	"os"
	"testing"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakes"
)

func TestMain(m *testing.M) {
	fakes.MaybeRun()
	os.Exit(m.Run())
}
