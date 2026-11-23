package agent

import (
	"context"
	"errors"
	"time"
)

// Backend answers completions through one provider's CLI.
type Backend interface {
	// Provider is the stable identifier stored with subscriptions ("claude").
	Provider() string
	// Label is the provider name shown to people ("Claude").
	Label() string
	// Run executes one completion for an account.
	Run(ctx context.Context, acc *Account, opts RunOptions, cb Callbacks) (*Result, error)
	// Login describes the CLI sign-in for an account.
	Login(acc *Account) LoginSpec
	// Logout removes credentials the CLI stored itself (saved tokens are
	// handled by the TokenStore).
	Logout(ctx context.Context, acc *Account) error
	// ValidateCredential checks a pasted token or API key.
	ValidateCredential(credential string) error
	// CredentialEnv is the environment variable that carries a saved credential.
	CredentialEnv() string
	// Models lists the model names advertised for this provider.
	Models() []string
	// DefaultModel is used when a request names no model this provider knows.
	DefaultModel() string
	// Accepts reports whether a model name belongs to this provider.
	Accepts(model string) bool
	// Version reports the CLI version.
	Version(ctx context.Context, env []string) string
}

// Account is one subscription's runtime: identity, private home directory,
// saved credential and status.
type Account struct {
	ID       string
	Name     string
	Provider string
	Home     string
	Tokens   *TokenStore
	Status   *Status
	// BaseEnv returns the environment for this account's CLI without the
	// credential variable.
	BaseEnv func() []string
}

// Env returns the full environment for a run: the base environment plus the
// saved credential in the backend's variable.
func (a *Account) Env(b Backend) []string {
	env := a.BaseEnv()
	if tok, _ := a.Tokens.Token(); tok != "" {
		env = append(env, b.CredentialEnv()+"="+tok)
	}
	return env
}

// FailFastWindow is how long a "login required" result is reused for new
// requests (with the same credential) before the CLI is tried again.
const FailFastWindow = 5 * time.Minute

// After a usage-limit or overload error an account is tried after the other
// candidates until these have passed.
const (
	RateLimitCooldown = 60 * time.Second
	OverloadCooldown  = 15 * time.Second
)

// Execute runs a completion on an account and records the outcome: success
// and failure counters and whether the login works. While the login is known
// to be broken, requests fail immediately without starting the CLI.
func Execute(ctx context.Context, b Backend, acc *Account, opts RunOptions, cb Callbacks) (*Result, error) {
	version := acc.Tokens.Version()
	if !opts.Probe {
		if reason, ok := acc.Status.LoginKnownBroken(version, FailFastWindow); ok {
			return nil, &Error{Kind: "auth", Status: 401, Message: reason, Subscription: acc.ID}
		}
	}
	res, err := b.Run(ctx, acc, opts, cb)
	source := "request"
	if opts.Probe {
		source = "check"
	}
	var ce *Error
	switch {
	case err == nil:
		acc.Status.RecordSuccess()
		acc.Status.RecordAuth(true, "", version, source)
	case errors.As(err, &ce):
		ce.Subscription = acc.ID
		acc.Status.RecordError(ce.Message)
		switch ce.Kind {
		case "auth":
			acc.Status.RecordAuth(false, ce.Message, version, source)
		case "rate_limit":
			acc.Status.CoolDown(time.Now().Add(RateLimitCooldown))
		case "overloaded":
			acc.Status.CoolDown(time.Now().Add(OverloadCooldown))
		}
	case ctx.Err() == nil:
		acc.Status.RecordError(err.Error())
	}
	return res, err
}
