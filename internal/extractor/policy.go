package extractor

import (
	"regexp"

	"github.com/microcosm-cc/bluemonday"
)

// anyValue allows any string (including newlines). Safe because bluemonday
// HTML-escapes attribute values when serialising; the matcher just gates
// content, it doesn't influence escaping.
var anyValue = regexp.MustCompile(`(?s).*`)

// srcsetValue matches a comma-separated list of "<url> <descriptor>" pairs
// where each URL is http(s) or protocol-relative. Rejects javascript:/data:
// schemes that bluemonday's URL validation would also reject for src.
var srcsetValue = regexp.MustCompile(
	`^\s*(?:(?:https?:)?//[^\s,]+(?:\s+[0-9.]+[wx])?\s*,?\s*)+$`,
)

// defaultPolicy extends bluemonday's UGCPolicy with broader <img> attribute
// support. UGCPolicy's stock `alt` regex rejects values containing colons,
// semicolons, and other common punctuation — which strips alt text from any
// article whose captions use a colon (very common). It also strips srcset,
// sizes, loading and decoding entirely, leaving feed readers without
// responsive images.
func defaultPolicy() *bluemonday.Policy {
	p := bluemonday.UGCPolicy()
	p.AllowAttrs("alt").Matching(anyValue).OnElements("img")
	p.AllowAttrs("srcset").Matching(srcsetValue).OnElements("img")
	p.AllowAttrs("sizes").Matching(anyValue).OnElements("img")
	p.AllowAttrs("loading").Matching(regexp.MustCompile(`^(lazy|eager)$`)).OnElements("img")
	p.AllowAttrs("decoding").Matching(regexp.MustCompile(`^(sync|async|auto)$`)).OnElements("img")
	return p
}
