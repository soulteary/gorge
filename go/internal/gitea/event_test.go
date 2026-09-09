package gitea

import (
	"reflect"
	"strings"
	"testing"
)

func TestEventTaskIDsAreUniqueAndSorted(t *testing.T) {
	event := Event{Text: "Fix T9, relates to t2 and T9; not XT7 or T0."}
	want := []string{"T2", "T9"}
	if got := event.TaskIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("TaskIDs() = %#v, want %#v", got, want)
	}
}

func TestTrustedURLRejectsAnotherOrigin(t *testing.T) {
	if got := trustedURL("https://git.example.com", "https://evil.example/T1"); got != "" {
		t.Fatalf("trustedURL() = %q", got)
	}
	if got := trustedURL("https://git.example.com", "https://git.example.com/team/repo/pulls/1"); got == "" {
		t.Fatal("expected URL from configured Gitea origin")
	}
}

func TestCommentCarriesDeliveryMarker(t *testing.T) {
	comment := (Event{DeliveryID: "delivery-1", Kind: "pull_request"}).Comment()
	if !strings.Contains(comment, "Gitea-Delivery: delivery-1") {
		t.Fatalf("comment does not carry idempotency marker: %q", comment)
	}
}
