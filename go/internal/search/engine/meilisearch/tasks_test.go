package meilisearch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
)

func TestIndexWaitsForItsTaskAndPropagatesFailure(t *testing.T) {
	for _, terminal := range []string{"succeeded", "failed", "canceled", "unexpected"} {
		t.Run(terminal, func(t *testing.T) {
			polls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "POST" {
					if _, err := fmt.Fprint(w, `{"taskUid":0}`); err != nil {
						t.Error(err)
						return
					}
					return
				}
				if r.URL.Path != "/tasks/0" {
					t.Errorf("wrong task path: %s", r.URL.Path)
				}
				polls++
				status := "processing"
				if polls > 1 {
					status = terminal
				}
				if _, err := fmt.Fprintf(w, `{"uid":0,"status":%q}`, status); err != nil {
					t.Error(err)
					return
				}
			}))
			defer srv.Close()
			err := newBackend(hostFor(srv.URL)).IndexDocument(&contracts.Document{PHID: "PHID-TASK-test", Type: "TASK"})
			if (err == nil) != (terminal == "succeeded") {
				t.Fatalf("terminal %s: %v", terminal, err)
			}
			if polls != 2 {
				t.Fatalf("polls=%d", polls)
			}
		})
	}
}

func TestTaskContractRejectsMissingOrWrongReceipts(t *testing.T) {
	for _, receipt := range []string{`{}`, `{"taskUid":-1}`, `{"taskUid":"1"}`} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := fmt.Fprint(w, receipt); err != nil {
				t.Error(err)
				return
			}
		}))
		_, err := newBackend(hostFor(srv.URL)).SubmitDocument(context.Background(), &contracts.Document{})
		srv.Close()
		if err == nil {
			t.Fatalf("accepted malformed receipt: %s", receipt)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, `{"uid":9,"status":"succeeded"}`); err != nil {
			t.Error(err)
			return
		}
	}))
	defer srv.Close()
	if _, err := newBackend(hostFor(srv.URL)).TaskState(context.Background(), 1); err == nil {
		t.Fatal("wrong receipt accepted")
	}
}

func TestWaitTaskTimeoutCannotReportIndexed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprint(w, `{"uid":1,"status":"processing"}`); err != nil {
			t.Error(err)
			return
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err := newBackend(hostFor(srv.URL)).WaitTask(ctx, 1)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "completion unknown") {
		t.Fatalf("timeout=%v", err)
	}
}
