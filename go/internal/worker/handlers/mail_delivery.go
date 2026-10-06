package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/worker"
)

var errMailCancellationRace = errors.New("mail cancellation raced with submission")

type mailRef struct {
	MailID     int64  `json:"mailID"`
	DeliveryID string `json:"deliveryID"`
	AllowSend  bool   `json:"allowSend"`
}

func mailDeliveryCall(ctx context.Context, url, token string, payload any) (*contracts.MailDeliveryResult, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Service-Token", token)
	response, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode == 409 {
		if strings.HasSuffix(url, "/cancel") {
			return nil, errMailCancellationRace
		}
		return nil, &worker.PermanentError{Msg: "immutable mail delivery collision"}
	}
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("mail delivery service HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data contracts.MailDeliveryResult `json:"data"`
	}
	if err = json.Unmarshal(body, &envelope); err != nil {
		return nil, err
	}
	if envelope.Data.DeliveryID == "" {
		return nil, fmt.Errorf("invalid mail delivery result")
	}
	return &envelope.Data, nil
}

// Preparation is the only stage which invokes PHP domain generation.
func NewMailPreparationHandler(conduit *ConduitClient, url, token string) worker.TaskHandler {
	url = strings.TrimRight(url, "/")
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		var id int64
		if err := json.Unmarshal(data, &id); err != nil || id <= 0 {
			return &worker.PermanentError{Msg: "invalid mail identity"}
		}
		prepared, err := conduit.Call(ctx, "mail.delivery", map[string]any{"phase": "prepare", "mailID": id})
		if err != nil {
			return err
		}
		var state struct {
			State string `json:"state"`
		}
		if err = json.Unmarshal(prepared.Result, &state); err != nil {
			return err
		}
		if state.State == "void" {
			return nil
		}
		var snapshot contracts.MailDeliveryRequest
		if err = json.Unmarshal(prepared.Result, &snapshot); err != nil {
			return err
		}
		if snapshot.SchemaVersion != 1 || snapshot.MailID != id || snapshot.DeliveryID == "" {
			return &worker.PermanentError{Msg: "invalid prepared mail snapshot"}
		}
		if snapshot.MailerURI != "" && strings.TrimRight(snapshot.MailerURI, "/") != strings.TrimRight(url, "/") {
			return fmt.Errorf("configured worker mailer differs from prepared delivery route")
		}
		result, err := mailDeliveryCall(ctx, url+"/api/mailer/prepare", token, snapshot)
		if err != nil {
			return err
		}
		if result.DeliveryID != snapshot.DeliveryID {
			return fmt.Errorf("delivery identity mismatch")
		}
		ref, _ := json.Marshal(mailRef{MailID: id, DeliveryID: snapshot.DeliveryID})
		return &worker.Completion{Followups: []contracts.EnqueueRequest{{TaskClass: "GorgeMailSubmitWorker", Data: string(ref), Priority: &task.Priority}}}
	}
}

// Submission retries only call the ledger and the idempotent result projector,
// never PHP execute/prepare. Projection succeeds before queue finalization.
func NewMailSubmitHandler(conduit *ConduitClient, url, token, policyPath string) worker.TaskHandler {
	url = strings.TrimRight(url, "/")
	return func(ctx context.Context, task *contracts.Task, data json.RawMessage) error {
		var ref mailRef
		if err := json.Unmarshal(data, &ref); err != nil || ref.MailID <= 0 || ref.DeliveryID == "" {
			return &worker.PermanentError{Msg: "invalid mail reference"}
		}
		policy, err := readFeedPolicy(policyPath)
		if err != nil {
			return err
		}
		ref.AllowSend = !*policy.Silent
		authorized, err := conduit.Call(ctx, "mail.delivery", map[string]any{"phase": "authorize", "mailID": ref.MailID})
		if err != nil {
			return err
		}
		var gate struct {
			Changed *bool `json:"changed"`
		}
		if err = json.Unmarshal(authorized.Result, &gate); err != nil || gate.Changed == nil {
			return fmt.Errorf("invalid recipient authorization")
		}
		if *gate.Changed {
			ref.AllowSend = false
		}
		result, err := mailDeliveryCall(ctx, url+"/api/mailer/execute", token, ref)
		if err != nil {
			return err
		}
		if *gate.Changed && (result.State == "prepared" || result.State == "retry_wait") {
			result, err = mailDeliveryCall(ctx, url+"/api/mailer/cancel", token, ref)
			if errors.Is(err, errMailCancellationRace) {
				result, err = mailDeliveryCall(ctx, url+"/api/mailer/execute", token, ref)
			}
			if err != nil {
				return err
			}
		}
		if result.DeliveryID != ref.DeliveryID {
			return fmt.Errorf("delivery identity mismatch")
		}
		raw, _ := json.Marshal(result)
		if err = applyMailResult(ctx, conduit, ref.MailID, string(raw)); err != nil {
			return err
		}
		switch result.State {
		case "accepted", "failed", "unknown", "expired", "cancelled":
			return nil
		case "submitting", "prepared", "retry_wait":
			return &worker.YieldError{Duration: int(max(5, result.NextAttempt-time.Now().Unix())), Msg: "mail delivery pending"}
		default:
			return fmt.Errorf("unknown mail delivery state")
		}
	}
}

func ValidateMailDeliveryService(ctx context.Context, url, token string) error {
	req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(url, "/")+"/api/mailer/delivery-capabilities", nil)
	if err != nil {
		return err
	}
	req.Header.Set("X-Service-Token", token)
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return fmt.Errorf("native mailer unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("native mailer capabilities unavailable: HTTP %d", response.StatusCode)
	}
	var envelope struct {
		Data struct {
			Version  int  `json:"schemaVersion"`
			Recovery bool `json:"recovery"`
		} `json:"data"`
	}
	if err = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&envelope); err != nil || envelope.Data.Version != 1 || !envelope.Data.Recovery {
		return fmt.Errorf("native mailer protocol incompatible")
	}
	return nil
}
