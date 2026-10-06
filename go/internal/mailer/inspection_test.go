package mailer

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/soulteary/gorge/go/internal/platform/httpx"
)

func TestDeliveryInspection(t *testing.T) {
	for _, tc := range []struct {
		name, state, receipt string
		pending              bool
	}{
		{"prepared", "prepared", `{}`, false},
		{"unknown", "unknown", `{"deliveryID":"mail/1","state":"unknown","revision":2}`, true},
		{"accepted", "accepted", `{"deliveryID":"mail/1","state":"accepted","revision":2,"mailerKey":"smtp","messageId":"receipt"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT state,revision,attempt,nextAttempt,result").WithArgs("mail/1").WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "attempt", "nextAttempt", "result", "deadline", "startedEpoch", "projectionPending", "projectionAttempts", "projectionNextAttempt", "projectionLastError"}).AddRow(tc.state, 2, 1, 0, tc.receipt, 400, 100, tc.pending, 3, 300, "projection acknowledgment unavailable"))
			mock.ExpectQuery("SELECT attempt,startedEpoch,finishedEpoch,outcome").WithArgs("mail/1").WillReturnRows(sqlmock.NewRows([]string{"attempt", "startedEpoch", "finishedEpoch", "outcome"}).AddRow(1, 100, nil, tc.state))
			mock.ExpectCommit()
			app := httpx.New(httpx.Config{}).App()
			RegisterRoutes(app, &Deps{Token: "internal", Delivery: &DeliveryService{DB: db}})
			req := httptest.NewRequest(http.MethodGet, "/api/mailer/delivery?deliveryID=mail%2F1", nil)
			req.Header.Set("X-Service-Token", "internal")
			response, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := response.Body.Close(); err != nil {
					t.Error(err)
				}
			}()
			if response.StatusCode != 200 {
				t.Fatalf("HTTP %d", response.StatusCode)
			}
			var envelope struct {
				Data DeliveryInspection `json:"data"`
			}
			if err = json.NewDecoder(response.Body).Decode(&envelope); err != nil {
				t.Fatal(err)
			}
			out := envelope.Data
			if out.Result.State != tc.state || out.Result.DeliveryID != "mail/1" || out.ProjectionPending != tc.pending || out.ProjectionAttempts != 3 || len(out.Attempts) != 1 || out.Attempts[0].FinishedEpoch != nil {
				t.Fatalf("unexpected inspection: %+v", out)
			}
			if tc.state == "accepted" && (out.Result.MailerKey != "smtp" || out.Result.MessageID != "receipt") {
				t.Fatal("lost provider receipt")
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeliveryInspectionErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"missing", sql.ErrNoRows, 404},
		{"store", sql.ErrConnDone, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				mock.ExpectClose()
				if err := db.Close(); err != nil {
					t.Error(err)
				}
			}()
			mock.ExpectBegin()
			mock.ExpectQuery("SELECT state,revision").WithArgs("mail/1").WillReturnError(tc.err)
			mock.ExpectRollback()
			app := httpx.New(httpx.Config{}).App()
			RegisterRoutes(app, &Deps{Token: "internal", Delivery: &DeliveryService{DB: db}})
			req := httptest.NewRequest(http.MethodGet, "/api/mailer/delivery?deliveryID=mail%2F1", nil)
			req.Header.Set("X-Service-Token", "internal")
			response, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(response.Body)
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != tc.status {
				t.Fatalf("HTTP %d: %s", response.StatusCode, body)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeliveryInspectionAuthenticationAndIdentity(t *testing.T) {
	app := httpx.New(httpx.Config{}).App()
	RegisterRoutes(app, &Deps{Token: "internal", Delivery: &DeliveryService{}})
	for _, tc := range []struct {
		path, token string
		status      int
	}{
		{"/api/mailer/delivery?deliveryID=mail%2F1", "", 401},
		{"/api/mailer/delivery-capabilities", "", 401},
		{"/api/mailer/delivery", "internal", 400},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("X-Service-Token", tc.token)
		response, err := app.Test(req)
		if err != nil {
			t.Fatal(err)
		}
		if err := response.Body.Close(); err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != tc.status {
			t.Fatalf("%s: HTTP %d", tc.path, response.StatusCode)
		}
	}
}
