package contracts

// GiteaMeta describes the intentionally narrow integration boundary.
type GiteaMeta struct {
	Direction string   `json:"direction"`
	Events    []string `json:"events"`
	Identity  string   `json:"identity"`
}

// GiteaDeliveryResult reports which task references an inbound delivery found.
type GiteaDeliveryResult struct {
	DeliveryID string   `json:"deliveryId"`
	Event      string   `json:"event"`
	Linked     []string `json:"linkedTasks"`
	Skipped    []string `json:"skippedTasks"`
}
