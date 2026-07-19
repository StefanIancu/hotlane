// Package respdiff normalizes HTTP response bodies so that behavioral
// comparison - drift checks, traffic replay - never reads legitimate
// volatility as divergence. Two builds of the same source may differ in
// timestamps, request ids, and epoch counters; what they must not differ
// in is everything else.
package respdiff

import "regexp"

// volatile are content patterns that legitimately vary between two runs
// of the same source: masked before bodies are compared. Order matters -
// timestamps go before the bare-number rule.
var volatile = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:?\d{2})?`), "<ts>"},                                                // ISO 8601
	{regexp.MustCompile(`(Mon|Tue|Wed|Thu|Fri|Sat|Sun), \d{2} (Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec) \d{4} \d{2}:\d{2}:\d{2} GMT`), "<ts>"}, // RFC 1123
	{regexp.MustCompile(`\b[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}\b`), "<uuid>"},
	{regexp.MustCompile(`\b[0-9a-fA-F]{16,64}\b`), "<hex>"}, // request ids, hashes
	{regexp.MustCompile(`\b\d{10,19}\b`), "<num>"},          // unix seconds/millis, counters
}

// Normalize masks volatile content so it never reads as drift.
func Normalize(body string) string {
	for _, v := range volatile {
		body = v.re.ReplaceAllString(body, v.repl)
	}
	return body
}

// Truncate bounds a body for human-facing diff detail.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
