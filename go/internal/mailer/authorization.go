package mailer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/soulteary/gorge/go/internal/contracts"
	"github.com/soulteary/gorge/go/internal/platform/conduitclient"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Both serialized message and complete form-encoded Conduit request are
// independently bounded by the existing 10MiB transport profile. Escaping and
// metadata mean not every raw message near that size is representable on wire.
const AttemptAuthorizationMessageLimit = 10 << 20
const AttemptAuthorizationRequestLimit = 10 << 20

// Fixed deployment-owned callback, never a URL from an immutable mail payload.
func NewAttemptAuthorizer(uri, token string) (func(context.Context, contracts.MailDeliveryRequest) error, error) {
	parsed, err := url.Parse(uri)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || token == "" || len(token) > 4096 {
		return nil, fmt.Errorf("mail authorization configuration refused")
	}
	client := conduitclient.NewBoundedRequest(strings.TrimRight(uri, "/"), token, 4096, AttemptAuthorizationRequestLimit)
	return func(ctx context.Context, request contracts.MailDeliveryRequest) error {
		binding := request.Authorization
		if request.MailID <= 0 || request.DeliveryID == "" || len(request.DeliveryID) > 128 || binding == nil || binding.Version != 1 || binding.MailID != request.MailID || binding.DeliveryID != request.DeliveryID || !regexp.MustCompile(`\A[a-f0-9]{64}\z`).MatchString(binding.MessageHash) {
			return permanentf("mail authorization binding refused")
		}
		raw, err := json.Marshal(request.Message)
		if err != nil || len(raw) > AttemptAuthorizationMessageLimit {
			return permanentf("mail authorization message refused")
		}
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		response, err := client.Call(ctx, "mail.delivery", map[string]any{"phase": "authorize-attempt", "mailID": request.MailID, "deliveryID": request.DeliveryID, "messageHash": binding.MessageHash, "message": string(raw)})
		if errors.Is(err, conduitclient.ErrRequestTooLarge) {
			return permanentf("mail authorization request exceeds transport budget")
		}
		if err != nil {
			return &SafeRetryError{Err: fmt.Errorf("mail authorization unavailable"), Backend: true}
		}
		var gate struct {
			Allowed *bool `json:"allowed"`
		}
		if json.Unmarshal(response.Result, &gate) != nil || gate.Allowed == nil {
			return &SafeRetryError{Err: fmt.Errorf("mail authorization invalid"), Backend: true}
		}
		if !*gate.Allowed {
			return permanentf("mail authorization refused")
		}
		return nil
	}, nil
}
