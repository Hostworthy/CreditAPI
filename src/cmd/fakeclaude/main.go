// Command fakeclaude is a fake Claude Code CLI for local end-to-end testing
// (make e2e, Bruno). Point CLAUDE_BIN at it; see src/tests/support/fakeclaude
// for how it behaves. It never contacts Anthropic.
package main

import (
	"os"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakeclaude"
)

func main() {
	os.Exit(fakeclaude.Main(os.Args[1:]))
}
