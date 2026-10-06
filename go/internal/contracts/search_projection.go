package contracts

// SearchProjection is a complete external-fulltext snapshot, not a PHP worker
// request. Revision is allocated by a durable per-object materializer; callers
// must never derive it from time or taskqueue IDs.
type SearchProjection struct {
	ProjectionVersion int       `json:"projectionVersion"`
	EventID           string    `json:"eventID"`
	Namespace         string    `json:"namespace"`
	PHID              string    `json:"phid"`
	Type              string    `json:"type"`
	Revision          string    `json:"revision"`
	Operation         string    `json:"operation"`
	SerializerVersion string    `json:"serializerVersion"`
	SourceVersion     string    `json:"sourceVersion"`
	PayloadHash       string    `json:"payloadHash"`
	Document          *Document `json:"document,omitempty"`
}
