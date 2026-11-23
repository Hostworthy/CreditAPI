package openai

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
)

// minOutputTokens keeps tiny max_tokens values from starving tool calls and
// structured output.
const minOutputTokens = 256

// stopFilter truncates streamed text at the first stop sequence.
type stopFilter struct {
	stops  []string
	maxLen int
	held   string
	hit    bool
}

func newStopFilter(stops []string) *stopFilter {
	f := &stopFilter{stops: stops}
	for _, s := range stops {
		f.maxLen = max(f.maxLen, len(s))
	}
	return f
}

// push returns text that is safe to emit and whether a stop sequence was hit.
func (f *stopFilter) push(s string) (string, bool) {
	if f.hit {
		return "", true
	}
	if len(f.stops) == 0 {
		return s, false
	}
	buf := f.held + s
	cut := -1
	for _, st := range f.stops {
		if i := strings.Index(buf, st); i >= 0 && (cut < 0 || i < cut) {
			cut = i
		}
	}
	if cut >= 0 {
		f.hit = true
		f.held = ""
		return buf[:cut], true
	}
	keep := f.maxLen - 1
	if keep <= 0 || len(buf) <= keep {
		if keep <= 0 {
			f.held = ""
			return buf, false
		}
		f.held = buf
		return "", false
	}
	split := len(buf) - keep
	for split > 0 && !utf8.RuneStart(buf[split]) {
		split--
	}
	f.held = buf[split:]
	return buf[:split], false
}

func (f *stopFilter) flush() string {
	s := f.held
	f.held = ""
	return s
}

func (h *Handler) runWithStops(ctx context.Context, a *attempt, cb agent.Callbacks, onText func(string)) (*agent.Result, string, error) {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	filter := newStopFilter(a.stops)
	var text strings.Builder
	cb.Text = func(delta string) {
		out, hit := filter.push(delta)
		if out != "" {
			text.WriteString(out)
			if onText != nil {
				onText(out)
			}
		}
		if hit {
			cancelRun()
		}
	}
	res, err := a.run(runCtx, cb)
	if filter.hit && ctx.Err() == nil {
		// Stopped on purpose at a stop sequence.
		if res == nil {
			res = &agent.Result{}
		}
		res.Text = text.String()
		res.FinishReason = "stop"
		res.ToolCalls = nil
		return res, text.String(), nil
	}
	if err != nil {
		return nil, "", err
	}
	if rest := filter.flush(); rest != "" {
		text.WriteString(rest)
		if onText != nil {
			onText(rest)
		}
	}
	return res, text.String(), nil
}
