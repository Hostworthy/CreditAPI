package openai

import (
	"context"
	"errors"
	"log/slog"
	"strings"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
	"github.com/atb00ker/CreditAPI/src/internal/subscriptions"
)

// attempt runs a request on its candidate subscriptions in order. It moves
// to the next candidate when one fails in a way another subscription might
// not (signed out, rate limited, overloaded, CLI failure), as long as nothing
// has been sent to the client yet.
type attempt struct {
	h      *Handler
	cands  []subscriptions.Candidate
	opts   agent.RunOptions
	stops  []string
	served subscriptions.Candidate
	tries  int
}

func (a *attempt) run(ctx context.Context, cb agent.Callbacks) (*agent.Result, error) {
	emitted := false
	wrapped := agent.Callbacks{
		Text: func(s string) {
			emitted = true
			if cb.Text != nil {
				cb.Text(s)
			}
		},
		ToolStart: func(i int, id, name string) {
			emitted = true
			if cb.ToolStart != nil {
				cb.ToolStart(i, id, name)
			}
		},
		ToolArgs: cb.ToolArgs,
	}
	var signedOut []subscriptions.Candidate
	var firstErr error
	// Subscriptions resting after a usage limit or overload go last; they are
	// still tried when nothing else answers.
	cands := make([]subscriptions.Candidate, 0, len(a.cands))
	var resting []subscriptions.Candidate
	for _, c := range a.cands {
		if c.Account.Status.CoolingDown() {
			resting = append(resting, c)
		} else {
			cands = append(cands, c)
		}
	}
	cands = append(cands, resting...)
	for _, c := range cands {
		a.served = c
		a.tries++
		opts := a.opts
		opts.Model = c.Model
		res, err := agent.Execute(ctx, c.Backend, c.Account, opts, wrapped)
		if err == nil {
			return res, nil
		}
		var ce *agent.Error
		if ctx.Err() != nil || emitted || !errors.As(err, &ce) || !ce.Retryable() {
			return nil, err
		}
		if ce.Kind == agent.KindAuth {
			signedOut = append(signedOut, c)
		} else if firstErr == nil {
			firstErr = err
		}
		logging.From(ctx).Info(logging.Event("chat", "subscription", "skipped"), slog.String("subscription", c.Sub.ID),
			slog.String("kind", ce.Kind))
	}
	if len(signedOut) > 0 {
		names := make([]string, len(signedOut))
		for i, c := range signedOut {
			names[i] = c.Sub.Name
		}
		msg := names[0] + " is signed out or its credential expired"
		if len(names) > 1 {
			msg = strings.Join(names, ", ") + " are signed out or their credentials expired"
		}
		return nil, &agent.Error{Kind: agent.KindAuth, Status: 401, Subscription: signedOut[0].Sub.ID, Message: msg}
	}
	return nil, firstErr
}
