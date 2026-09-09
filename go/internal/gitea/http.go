package gitea

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strings"

	"github.com/gofiber/fiber/v3"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/auth"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

const CodeDeliveryFailed = "ERR_GITEA_DELIVERY_FAILED"

var deliveryPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

type Deps struct {
	Sink          Sink
	Token         string
	WebhookSecret string
	BaseURL       string
}

func RegisterRoutes(app fiber.Router, deps *Deps) {
	app.Post("/webhooks/gitea", receive(deps))
	api := app.Group("/api/gitea")
	api.Use(auth.Token(deps.Token, auth.WithQueryToken(false)))
	api.Get("/meta", func(c fiber.Ctx) error {
		return httpx.OK(c, contracts.GiteaMeta{
			Direction: "gitea-to-phorge",
			Events:    []string{"issues", "pull_request", "push", "release"},
			Identity:  "stargate",
		})
	})
}

func receive(deps *Deps) fiber.Handler {
	return func(c fiber.Ctx) error {
		body := c.Body()
		if !validSignature(body, c.Get("X-Gitea-Signature"), deps.WebhookSecret) {
			return httpx.Fail(c, http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid webhook signature")
		}
		delivery := strings.TrimSpace(c.Get("X-Gitea-Delivery"))
		kind := strings.TrimSpace(c.Get("X-Gitea-Event"))
		if !deliveryPattern.MatchString(delivery) || kind == "" {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "missing Gitea delivery headers")
		}

		var payload struct {
			Action string `json:"action"`
			Sender struct {
				Login string `json:"login"`
			} `json:"sender"`
			Repository struct {
				FullName string `json:"full_name"`
				HTMLURL  string `json:"html_url"`
			} `json:"repository"`
			Issue struct {
				Title   string `json:"title"`
				Body    string `json:"body"`
				HTMLURL string `json:"html_url"`
			} `json:"issue"`
			PullRequest struct {
				Title   string `json:"title"`
				Body    string `json:"body"`
				HTMLURL string `json:"html_url"`
			} `json:"pull_request"`
			Release struct {
				Name    string `json:"name"`
				Body    string `json:"body"`
				HTMLURL string `json:"html_url"`
			} `json:"release"`
			Commits []struct {
				Message string `json:"message"`
				URL     string `json:"url"`
			} `json:"commits"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "invalid Gitea payload")
		}
		if !supportedEvent(kind) {
			return httpx.Fail(c, http.StatusBadRequest, httpx.CodeBadRequest, "unsupported Gitea event")
		}
		text := strings.Join([]string{payload.Issue.Title, payload.Issue.Body, payload.PullRequest.Title, payload.PullRequest.Body, payload.Release.Name, payload.Release.Body}, "\n")
		candidateURL := firstNonEmpty(payload.PullRequest.HTMLURL, payload.Issue.HTMLURL, payload.Release.HTMLURL)
		for _, commit := range payload.Commits {
			text += "\n" + commit.Message
			if candidateURL == "" {
				candidateURL = commit.URL
			}
		}
		event := Event{DeliveryID: delivery, Kind: kind, Action: payload.Action, Repository: payload.Repository.FullName, Actor: payload.Sender.Login, URL: trustedURL(deps.BaseURL, candidateURL), Text: text}
		result := contracts.GiteaDeliveryResult{DeliveryID: delivery, Event: kind, Linked: []string{}, Skipped: []string{}}
		for _, task := range event.TaskIDs() {
			posted, err := deps.Sink.Append(c.Context(), task, event)
			if err != nil {
				slog.Error("GITEA_DELIVERY_FAILED", "delivery_id", delivery, "event", kind, "task", task, "error", err)
				return httpx.Fail(c, http.StatusBadGateway, CodeDeliveryFailed, "Phorge rejected the Gitea event")
			}
			if posted {
				result.Linked = append(result.Linked, task)
			} else {
				result.Skipped = append(result.Skipped, task)
			}
		}
		return httpx.OK(c, result)
	}
}

func validSignature(body []byte, presented, secret string) bool {
	if presented == "" || secret == "" {
		return false
	}
	expected := hmac.New(sha256.New, []byte(secret))
	_, _ = expected.Write(body)
	presentedBytes, err := hex.DecodeString(strings.TrimPrefix(presented, "sha256="))
	return err == nil && hmac.Equal(expected.Sum(nil), presentedBytes)
}

func supportedEvent(kind string) bool {
	switch kind {
	case "issues", "pull_request", "push", "release":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
