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
	{regexp.MustCompile(`\b[0-9a-fA-F]{32,64}\b`), "<hex>"}, // request ids, trace ids
	// Epoch-SHAPED only. `\d{10,19}` swallowed order numbers, account
	// numbers, phone numbers with country code and cent-denominated
	// balances - masking exactly the values a regression would change.
	// Unix seconds and millis both start with 1 for the next century.
	// 1[6-9]... keeps the window to ~2020-2033 rather than every
	// ten-digit number that happens to start with 1 (order ids, balances).
	{regexp.MustCompile(`\b1[6-9]\d{8}\b`), "<num>"},  // unix seconds
	{regexp.MustCompile(`\b1[6-9]\d{11}\b`), "<num>"}, // unix millis
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
