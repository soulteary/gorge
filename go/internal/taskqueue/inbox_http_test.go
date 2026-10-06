package taskqueue

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/soulteary/gorge/go/internal/contracts"
)

type inboxHTTPStore struct {
	Store
	err error
}

func (s *inboxHTTPStore) EnqueueEvent(context.Context, *contracts.EnqueueEventRequest) (*contracts.Task, error) {
	return &contracts.Task{ID: 7, TaskClass: "Feed"}, s.err
}
func TestInboxHTTPAuthenticationValidationAndCollision(t *testing.T) {
	store := &inboxHTTPStore{}
	app := newTestServer(t, store)
	resp, _ := dispatch(t, app, httptest.NewRequest("POST", "/api/queue/enqueue-event", nil))
	if resp.StatusCode != 401 {
		t.Fatal("inbox allowed unauthenticated writes")
	}
	for _, body := range []string{`{}`, `{"eventID":"event","task":{"data":"{}"}}`} {
		resp, body := post(t, app, "/api/queue/enqueue-event", body)
		if resp.StatusCode != 400 {
			t.Fatal(body)
		}
	}
	request := `{"eventID":"feed/17","task":{"taskClass":"Feed","data":"{}"}}`
	resp, body := post(t, app, "/api/queue/enqueue-event", request)
	if resp.StatusCode != 200 {
		t.Fatal(body)
	}
	store.err = ErrEventConflict
	resp, body = post(t, app, "/api/queue/enqueue-event", request)
	assertErrorCode(t, resp, body, 409, "ERR_EVENT_CONFLICT")
}
