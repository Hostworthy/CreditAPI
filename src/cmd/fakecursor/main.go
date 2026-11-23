// Command fakecursor is a fake Cursor CLI (cursor-agent) for local
// end-to-end testing (make e2e, Bruno). Point CURSOR_BIN at it; see
// src/tests/support/fakecursor for how it behaves. It never contacts Cursor.
package main

import (
	"os"

	"github.com/atb00ker/CreditAPI/src/tests/support/fakecursor"
)

func main() {
	os.Exit(fakecursor.Main(os.Args[1:]))
}
