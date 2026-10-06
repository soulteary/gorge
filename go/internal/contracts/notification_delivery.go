package contracts

import "encoding/json"

// NotificationPublishData is a durable worker envelope, distinct from the
// Aphlict browser message. Missing version identifies a pre-migration task.
type NotificationPublishData struct {
	DeliveryVersion int                        `json:"deliveryVersion"`
	EventID         string                     `json:"eventID"`
	Instance        string                     `json:"instance"`
	Message         map[string]json.RawMessage `json:"message"`
}
