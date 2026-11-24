package openai

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/atb00ker/CreditAPI/src/internal/agent"
	"github.com/atb00ker/CreditAPI/src/internal/httpx"
	"github.com/atb00ker/CreditAPI/src/internal/logging"
)

// loginURL is the address of the /login page shown in "login required"
// errors: PUBLIC_URL when set, otherwise the address the client used.
func (h *Handler) loginURL(r *http.Request) string {
	if h.cfg.PublicURL != "" {
		return h.cfg.PublicURL + "/login"
	}
	scheme := "http"
	if h.cfg.TrustedProxies.HTTPS(r) {
		scheme = "https"
	}
	return scheme + "://" + h.cfg.TrustedProxies.Host(r) + "/login"
}

func describeError(err error, loginURL string) (status int, typ, code, msg string) {
	var ce *agent.Error
	if errors.As(err, &ce) {
		switch ce.Kind {
		case agent.KindAuth:
			// 401 so OpenAI SDKs do not retry; the message carries the link.
			link := loginURL
			if ce.Subscription != "" {
				link += "?subscription=" + ce.Subscription
			}
			return http.StatusUnauthorized, "authentication_error", "subscription_login_required",
				"Sign-in required: " + ce.Message + ". Sign in at " + link + " and then try again."
		case agent.KindForbidden:
			return http.StatusForbidden, "permission_error", "no_subscription", ce.Message
		case agent.KindNotFound:
			return http.StatusNotFound, "invalid_request_error", "model_not_found", ce.Message
		case agent.KindRateLimit:
			return ce.Status, "rate_limit_error", "rate_limit_exceeded", ce.Message
		case agent.KindBusy:
			return ce.Status, "rate_limit_error", "server_busy", ce.Message
		case agent.KindOverloaded:
			return ce.Status, "service_unavailable", "overloaded", ce.Message
		case agent.KindInvalidRequest:
			return ce.Status, "invalid_request_error", "invalid_request", ce.Message
		case agent.KindInvalidModel:
			return http.StatusBadRequest, "invalid_request_error", "invalid_model", ce.Message
		}
		return ce.Status, "api_error", "upstream_error", ce.Message
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "api_error", "timeout", "request timed out (REQUEST_TIMEOUT)"
	}
	return http.StatusInternalServerError, "api_error", "internal_error", "internal server error; the server log has the details"
}

// clean removes secrets and local paths from a message sent to a client.
func (h *Handler) clean(msg string) string {
	msg = logging.RedactString(msg)
	for path, label := range map[string]string{h.cfg.DataDir: "<data>", os.TempDir(): "<tmp>"} {
		if path != "" && path != "/" {
			msg = strings.ReplaceAll(msg, path, label)
		}
	}
	return msg
}

func (h *Handler) fail(w http.ResponseWriter, err error, loginURL string) {
	h.failAs(openaiFormat{}, w, err, loginURL)
}

func (h *Handler) failAs(f format, w http.ResponseWriter, err error, loginURL string) {
	if errors.Is(err, context.Canceled) {
		return // client went away
	}
	status, typ, code, msg := describeError(err, loginURL)
	f.writeError(w, status, typ, code, h.clean(msg))
}

func writeJSON(w http.ResponseWriter, status int, v any) { httpx.WriteJSON(w, status, v) }

func writeError(w http.ResponseWriter, status int, typ, code, msg string) {
	writeJSON(w, status, ErrorBody{Error: ErrorDetail{Message: msg, Type: typ, Code: &code}})
}
