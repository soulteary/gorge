package gitea

import (
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

var taskPattern = regexp.MustCompile(`(?i)(?:^|[^A-Z0-9])T([1-9][0-9]*)\b`)

type Event struct {
	DeliveryID string
	Kind       string
	Action     string
	Repository string
	Actor      string
	URL        string
	Text       string
}

func (e Event) TaskIDs() []string {
	seen := make(map[string]struct{})
	for _, match := range taskPattern.FindAllStringSubmatch(e.Text, 100) {
		seen["T"+match[1]] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (e Event) Marker() string { return "Gitea-Delivery: " + e.DeliveryID }

func (e Event) Comment() string {
	lines := []string{fmt.Sprintf("Gitea `%s` event", e.Kind)}
	if e.Action != "" {
		lines[0] += fmt.Sprintf(" (`%s`)", e.Action)
	}
	if e.Repository != "" {
		lines = append(lines, "Repository: `"+e.Repository+"`")
	}
	if e.Actor != "" {
		lines = append(lines, "Actor: `"+e.Actor+"`")
	}
	if e.URL != "" {
		lines = append(lines, "Source: "+e.URL)
	}
	lines = append(lines, "", e.Marker())
	return strings.Join(lines, "\n")
}

func trustedURL(base, candidate string) string {
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Scheme == "" || baseURL.Host == "" {
		return ""
	}
	candidateURL, err := url.Parse(candidate)
	if err != nil || candidateURL.Scheme != baseURL.Scheme || !strings.EqualFold(candidateURL.Host, baseURL.Host) {
		return ""
	}
	return candidateURL.String()
}
