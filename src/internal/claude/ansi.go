package claude

import (
	"regexp"
	"strings"
)

var (
	// OSC 8 hyperlink: ESC ] 8 ; params ; URL (BEL | ESC \)
	osc8Re = regexp.MustCompile(`\x1b\]8;[^;\x07\x1b]*;([^\x07\x1b]*)(?:\x07|\x1b\\)`)
	// Any other OSC sequence.
	oscRe = regexp.MustCompile(`\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)`)
	// Cursor movement that Ink uses instead of spaces (CHA / CUF).
	cursorSpaceRe = regexp.MustCompile(`\x1b\[\d*[GC]`)
	// Remaining CSI sequences (colors, erase, cursor up, private modes...).
	csiRe = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)
	// Two-character escapes and charset selection.
	escRe = regexp.MustCompile(`\x1b[()][0-9A-Za-z]|\x1b[=>78DEHMNOZc]`)

	urlRe   = regexp.MustCompile(`https://[^\s"'<>\x1b\x07]+`)
	tokenRe = regexp.MustCompile(`sk-ant-oat\d{2}-[A-Za-z0-9_\-]{20,}`)
	// Looser fallback in case the token format changes.
	tokenLooseRe = regexp.MustCompile(`sk-ant-[a-z]{2,8}\d{0,3}-[A-Za-z0-9_\-]{40,}`)
	spinnerRe    = regexp.MustCompile(`(?m)^[\s✢✳✶✻✽·*]+$`)
	blankRunsRe  = regexp.MustCompile(`\n{3,}`)
	boxRe        = regexp.MustCompile(`[╭╮╰╯─│]{3,}`)
)

// StripANSI converts raw TUI output into readable plain text.
func StripANSI(s string) string {
	s = osc8Re.ReplaceAllString(s, "")
	s = oscRe.ReplaceAllString(s, "")
	s = cursorSpaceRe.ReplaceAllString(s, " ")
	s = csiRe.ReplaceAllString(s, "")
	s = escRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return s
}

// TidyOutput makes captured CLI output pleasant to show in a browser:
// it removes spinner frames, long box-drawing borders and blank runs.
func TidyOutput(s string) string {
	s = StripANSI(s)
	s = boxRe.ReplaceAllString(s, "")
	s = spinnerRe.ReplaceAllString(s, "")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t│")
		lines[i] = strings.TrimLeft(lines[i], "│")
	}
	s = strings.Join(lines, "\n")
	s = blankRunsRe.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ExtractLoginURL finds the OAuth sign-in URL printed by `claude setup-token`.
// Hyperlink targets are preferred because they are never wrapped or truncated.
func ExtractLoginURL(raw string) string {
	for _, m := range osc8Re.FindAllStringSubmatch(raw, -1) {
		if isLoginURL(m[1]) {
			return m[1]
		}
	}
	for _, u := range urlRe.FindAllString(StripANSI(raw), -1) {
		if isLoginURL(u) {
			return strings.TrimRight(u, ".,)")
		}
	}
	return ""
}

func isLoginURL(u string) bool {
	return strings.HasPrefix(u, "https://") &&
		(strings.Contains(u, "oauth") || strings.Contains(u, "authorize"))
}

// ExtractToken finds a long-lived OAuth token in CLI output.
func ExtractToken(raw string) string {
	plain := StripANSI(raw)
	if t := tokenRe.FindString(plain); t != "" {
		return t
	}
	// The PTY is very wide, so the token is never wrapped; no need to
	// re-join lines (which could glue following text onto the token).
	return tokenLooseRe.FindString(plain)
}
