// Package pdftext is the PDF text-extraction helper and its wire protocol.
//
// mail-mcp contains no PDF parser: parsing untrusted PDFs in-process was
// removed after a security review (decompression bombs, page-tree loops, text
// amplification). The parsing happens in a separate container: this helper
// listens on a unix socket and, for each request, spawns one pdftotext
// (poppler) under hard limits, kills it when a limit is hit, and returns a
// bounded result. The helper itself parses nothing but a 64-hex digest and the
// five-byte "%PDF-" magic.
package pdftext

// Statuses of a Response.
const (
	StatusOK       = "ok"
	StatusTimeout  = "timeout"
	StatusTooLarge = "too_large"
	StatusFailed   = "failed"
	StatusNotPDF   = "not_pdf"
	// StatusUnavailable: the helper cannot do its job at all (pdftotext
	// missing or not startable, limits that cannot be applied, a failed
	// start-up self-test, a kill the file did not cause). It is about the
	// helper, never about the file: clients must not record it as an outcome.
	StatusUnavailable = "unavailable"
)

// Limits. They are constants, not flags: the point is that a deployment
// cannot loosen them by accident.
const (
	MaxInputBytes  = 25 << 20 // larger inputs are refused before anything is spawned
	MaxOutputBytes = 1 << 20  // read from pdftotext; the process is killed beyond it
	MaxPages       = 30       // pdftotext -l
	MaxConcurrent  = 2        // jobs at once
	maxRequestLine = 4 << 10
)

// Request names a blob by digest. The helper resolves the path itself.
type Request struct {
	Hash string `json:"hash"`
}

// Response is the result for one request.
type Response struct {
	Status string `json:"status"`
	Text   string `json:"text,omitempty"`
	// Truncated: the text was cut at MaxOutputBytes.
	Truncated bool `json:"truncated,omitempty"`
	// PagesCapped: the document has at least MaxPages pages, so pages past
	// the cap may exist and were not read.
	PagesCapped bool `json:"pages_capped,omitempty"`
}
