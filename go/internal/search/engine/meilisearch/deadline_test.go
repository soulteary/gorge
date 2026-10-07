package meilisearch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/search/engine"
)

type deadlineTransport func(*http.Request) (*http.Response, error)

func (f deadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCallerDeadlineBoundsMeilisearchSubmitAndConfirmation(t *testing.T) {
	for _, phase := range []string{"submit", "confirmation"} {
		t.Run(phase, func(t *testing.T) {
			b := New(engine.BackendDef{Hosts: []string{"meili.invalid"}, Timeout: 30})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
			defer cancel()
			deadline, _ := ctx.Deadline()
			posts, gets := 0, 0
			b.client = &http.Client{Transport: deadlineTransport(func(r *http.Request) (*http.Response, error) {
				got, ok := r.Context().Deadline()
				if !ok || !got.Equal(deadline) {
					t.Errorf("caller deadline was replaced: %v", got)
				}
				if r.Method == http.MethodPost {
					posts++
					if phase == "submit" {
						<-r.Context().Done()
						return nil, r.Context().Err()
					}
					return &http.Response{StatusCode: 202, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"taskUid":1}`))}, nil
				}
				gets++
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"uid":1,"status":"processing"}`))}, nil
			})}
			start := time.Now()
			err := b.IndexDocumentContext(ctx, &contracts.Document{PHID: "PHID-TASK-budget", Type: "TASK"})
			if !errors.Is(err, context.DeadlineExceeded) || posts != 1 || time.Since(start) > time.Second {
				t.Fatalf("Meilisearch escaped caller budget: posts=%d gets=%d elapsed=%v err=%v", posts, gets, time.Since(start), err)
			}
		})
	}
}
