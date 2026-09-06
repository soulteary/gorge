// Package contracts holds the wire types shared by the Go services, the PHP
// adapters, the OpenAPI documents and the language-neutral contract fixtures.
// It is the single source of truth for what goes over the network: nothing in
// here may carry behaviour, and every field change is a compatibility change.
package contracts

// HighlightRequest is the body of POST /api/highlight/render. An empty
// Language asks the renderer to detect the language from Source.
type HighlightRequest struct {
	Source   string `json:"source"`
	Language string `json:"language"`
}

// HighlightResult is the payload returned inside the response envelope's data
// field. HTML is a bare sequence of <span class="..."> elements using
// Pygments-compatible class names, with no surrounding <pre> or <div>.
type HighlightResult struct {
	HTML     string `json:"html"`
	Language string `json:"language"`
}
