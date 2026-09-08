// Package diff is the diff domain: it compares two texts for Phorge, either
// as a unified diff over lines or as a prose diff over running text.
//
// It shares the gorge-render binary with the render domain. Both are pure
// computation with no external dependency, so there is nothing to gain from a
// second process, and the routes are named after the domain rather than the
// binary precisely so this could happen without either side changing.
package diff

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/diff/prose"
	"github.com/soulteary/gorge/go/internal/diff/unified"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

// Deps is everything the diff routes need to serve a request. Both engines are
// stateless functions, so unlike render there is no engine to inject.
type Deps struct {
	Token    string
	MaxBytes int
}

// RegisterRoutes mounts the diff endpoints.
//
// This domain defines no error code of its own, which is worth stating because
// render does define one. Neither engine has a failure mode to report: the
// prose engine is total, and the only thing the unified engine rejects is an
// input too large to diff, which is already ERR_TOO_LARGE. A code invented
// here would never be returned.
func RegisterRoutes(app fiber.Router, deps *Deps) {
	g := app.Group("/api/diff")
	g.Use(auth.Token(deps.Token))

	g.Post("/generate", generateDiff(deps))
	g.Post("/prose", proseDiff(deps))
}

func generateDiff(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.DiffRequest
		if answered, err := bind(c, &req); answered {
			return err
		}

		if answered, err := checkSize(c, deps, len(req.Old)+len(req.New)); answered {
			return err
		}

		result, err := unified.Generate(&req)
		if err != nil {
			if errors.Is(err, unified.ErrTooLarge) {
				// A line-count blowup, not a byte-count one: the request
				// passed MaxBytes but asks for a comparison table too big to
				// allocate. Same code, because the caller's remedy is the
				// same — send less.
				return httpx.Fail(c, http.StatusRequestEntityTooLarge,
					httpx.CodeTooLarge, err.Error())
			}
			return httpx.Fail(c, http.StatusInternalServerError, httpx.CodeInternal, err.Error())
		}

		return httpx.OK(c, result)
	}
}

func proseDiff(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		var req contracts.ProseRequest
		if answered, err := bind(c, &req); answered {
			return err
		}

		if answered, err := checkSize(c, deps, len(req.Old)+len(req.New)); answered {
			return err
		}

		return httpx.OK(c, prose.Generate(&req))
	}
}

// Both helpers below report `answered` separately from `err`, and callers must
// branch on `answered` rather than on the error. httpx.Fail writes the response
// and normally returns nil, so a nil error out of a helper cannot distinguish
// "carry on" from "already replied". Reading it as the former appends a second
// JSON document to a response that was finished, which no test of the status
// code alone will notice.
//
// render/http.go has the same three checks inline in its handler, where
// `return httpx.Fail(...)` ends things by itself and the question never comes
// up. Two endpoints sharing them here is what makes the extra return value
// necessary.

// bind decodes the request body, keeping a transport-level rejection distinct
// from a malformed one.
//
// A body over the platform limit can surface here as Fiber's 413. Handing that
// back to the platform error handler keeps it reported as ERR_TOO_LARGE instead
// of flattening it into ERR_BAD_REQUEST, which callers branch on. Malformed
// JSON is a genuine 400 and is answered here.
func bind(c fiber.Ctx, req any) (answered bool, err error) {
	bindErr := c.Bind().Body(req)
	if bindErr == nil {
		return false, nil
	}

	var fiberErr *fiber.Error
	if errors.As(bindErr, &fiberErr) && fiberErr.Code != http.StatusBadRequest {
		return true, bindErr
	}
	return true, httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, bindErr.Error())
}

// checkSize rejects a comparison whose two sides together exceed the domain
// limit. The sides are summed rather than checked individually because the
// cost of a diff is driven by both.
func checkSize(c fiber.Ctx, deps *Deps, size int) (answered bool, err error) {
	if deps.MaxBytes > 0 && size > deps.MaxBytes {
		return true, httpx.Fail(c, http.StatusRequestEntityTooLarge,
			httpx.CodeTooLarge, "combined input exceeds maximum allowed size")
	}
	return false, nil
}
