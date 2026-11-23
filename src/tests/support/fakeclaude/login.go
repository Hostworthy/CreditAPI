package fakeclaude

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

const (
	// LoginURLPrefix starts every sign-in URL the fake prints. The state
	// parameter is random, so a new sign-in always gets a new URL.
	LoginURLPrefix = "https://claude.ai/oauth/authorize?code=true&client_id=fake-client&state="
	// CodePrefix marks authentication codes the fake accepts.
	CodePrefix = "fake-code"
)

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// setupToken imitates `claude setup-token` in a terminal: a banner drawn with
// cursor movement codes, the sign-in URL as an OSC 8 hyperlink, a prompt for
// the code and, for a code starting with CodePrefix, a new long-lived token.
func setupToken(stdin io.Reader, out io.Writer) int {
	url := LoginURLPrefix + randomHex(8)
	fmt.Fprint(out, "Welcome to Claude Code v0.0.0-fake\r\r\n\r\r\nThis\x1b[6Gwill\x1b[11Gguide\x1b[17Gyou\x1b[21Gthrough\x1b[29Glong-lived\x1b[40Gtoken\x1b[46Gsetup.\r\r\n")
	fmt.Fprint(out, "\x1b[>0q\r✢\r\r\n\r*\r\r\n")
	fmt.Fprint(out, "Browser didn't open?\x1b[23GUse the URL\x1b[35Gbelow\x1b[41Gto\x1b[44Gsign\x1b[49Gin\r\r\n\r\r\n")
	fmt.Fprintf(out, "\x1b]8;id=1;%s\x07\x1b[38;5;246m%s\x1b[39m\x1b]8;;\x07\r\r\n\r\r\n", url, url)
	fmt.Fprint(out, "\x1b[2GPaste\x1b[8Gcode\x1b[13Ghere\x1b[18Gif\x1b[21Gprompted\r\r\n\x1b[2G╭──────────────╮\r\r\n")

	// The terminal may be in canonical mode, which turns Enter into \n.
	var sb strings.Builder
	rd := bufio.NewReader(stdin)
	for {
		c, err := rd.ReadByte()
		if err != nil || c == '\r' || c == '\n' {
			break
		}
		sb.WriteByte(c)
	}
	code := strings.TrimSpace(sb.String())
	if !strings.HasPrefix(code, CodePrefix) {
		fmt.Fprint(out, "\r\r\n\x1b[31mOAuth error: Invalid code\x1b[39m\r\r\n")
		return 1
	}
	token := TokenPrefix + randomHex(24)
	fmt.Fprint(out, "\r\r\n\x1b[32m✓\x1b[39m Long-lived authentication token created successfully!\r\r\n\r\r\n")
	fmt.Fprintf(out, "Your OAuth token (valid for 1 year):\r\r\n\r\r\n\x1b[33m%s\x1b[39m\r\r\n\r\r\n", token)
	fmt.Fprint(out, "Store this token securely. You won't be able to see it again.\r\r\n")
	return 0
}
