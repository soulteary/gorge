// Package mailer is the outbound email domain: it takes one message from
// Phorge and hands it to whichever configured backend accepts it first. It owns
// no queue and no retry authority — Phorge's worker queue keeps both. See
// docs/modules/mailer.md.
package mailer

import (
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// Domain-specific codes, both predating the monorepo.
//
// CodePermanentFailure is the load-bearing one: Phorge's client turns it into a
// PhabricatorMetaMTAPermanentFailureException, which is what stops its worker
// from re-queueing a message no backend will ever accept. Collapsing it into a
// platform code would make every bad recipient address retry forever.
const (
	CodePermanentFailure = "ERR_PERMANENT_FAILURE"
	CodeSendFailed       = "ERR_SEND_FAILED"
)

// Deps is everything the mailer routes need to serve a request.
type Deps struct {
	Dispatcher *Dispatcher
	Token      string
	BodyLimit  int
}

// RegisterRoutes mounts the mailer endpoints.
//
// The paths are named after the domain and must not change: Phorge's
// PhabricatorGorgeMailerClient already calls them.
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/mailer")
	g.Use(auth.Token(deps.Token))

	g.Post("/send", sendMail(deps))
	g.Get("/mailers", listMailers(deps))
}

func sendMail(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.SendRequest
		if err := c.Bind().Body(&req); err != nil {
			// A body over the platform limit is rejected by fasthttp before
			// this handler runs and reaches the platform error handler as
			// fiber.ErrRequestEntityTooLarge, which keeps it reported as
			// ERR_TOO_LARGE rather than flattened into ERR_BAD_REQUEST. Any
			// non-400 *fiber.Error that still surfaces here is handed back for
			// the same reason. Malformed JSON is a genuine 400 and stays here.
			var fiberErr *fiber.Error
			if errors.As(err, &fiberErr) && fiberErr.Code != http.StatusBadRequest {
				return err
			}
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		}

		msg := &req.Message

		// These three are 400 rather than 422: a message this incomplete never
		// reached a backend, so nothing has judged it undeliverable. The caller
		// built a bad request.
		if msg.From.Address == "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "from address is required")
		}
		if len(msg.To) == 0 {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "at least one recipient is required")
		}
		if msg.Subject == "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "subject is required")
		}

		if deps.BodyLimit > 0 {
			msg.TextBody = truncateUTF8(msg.TextBody, deps.BodyLimit)
			msg.HTMLBody = truncateUTF8(msg.HTMLBody, deps.BodyLimit)
		}

		// The request context bounds the whole dispatch, retries included, so a
		// client that gave up stops the work it was waiting for.
		result, err := deps.Dispatcher.SendWith(c.Context(), msg, req.MailerKeys)
		if err != nil {
			if IsPermanent(err) {
				return httpx.Fail(c, http.StatusUnprocessableEntity, CodePermanentFailure, err.Error())
			}
			return httpx.Fail(c, http.StatusBadGateway, CodeSendFailed, err.Error())
		}

		return httpx.OK(c, result)
	}
}

func listMailers(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		return httpx.OK(c, deps.Dispatcher.MailerInfo())
	}
}

// truncateUTF8 cuts s to at most maxBytes without splitting a rune, so an
// oversized body is shortened rather than turned into invalid UTF-8 that a
// backend would reject or mangle.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
