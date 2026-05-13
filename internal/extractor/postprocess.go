package extractor

import (
	"strings"

	"golang.org/x/net/html"
)

var blockChildTags = map[string]bool{
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"figure": true, "ul": true, "ol": true, "blockquote": true,
	"table": true, "pre": true, "hr": true, "div": true, "p": true,
}

// simplifyDOM cleans up common structural quirks left over after readability
// extraction so the rendered HTML embeds cleanly in feed readers.
//
//   - strips data-* attributes (bluemonday drops them too; stripping early lets
//     us recognise wrapper divs/spans that are effectively attribute-less)
//   - unwraps <p> wrapping block-level elements (e.g. <p><h2>...</h2></p>),
//     which browsers auto-close into invalid sibling structures
//   - unwraps <p> directly inside <figure>/<picture> (common bad markup)
//   - unwraps <div>/<span> with no remaining attributes
//   - drops <img aria-label="image unavailable"> lazy-load placeholders that
//     would otherwise render as grey blocks next to the real image in feed
//     readers (which don't run the swap-in JS)
//
// Runs to a fixed point so cascades work (e.g. unwrapping an outer <div>
// reveals that an inner <p> is now directly inside a <figure>).
func simplifyDOM(n *html.Node) {
	for simplifyPass(n) {
	}
}

func simplifyPass(n *html.Node) bool {
	if n == nil {
		return false
	}
	changed := false
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if simplifyPass(c) {
			changed = true
		}
		c = next
	}
	if n.Type != html.ElementNode {
		return changed
	}
	if n.Data == "img" && isPlaceholderImage(n) && n.Parent != nil {
		n.Parent.RemoveChild(n)
		return true
	}
	n.Attr = stripDataAttrs(n.Attr)
	switch n.Data {
	case "p":
		if hasBlockChild(n) || figureParent(n) {
			unwrapNode(n)
			changed = true
		}
	case "div", "span":
		if len(n.Attr) == 0 && n.Parent != nil {
			unwrapNode(n)
			changed = true
		}
	}
	return changed
}

func hasBlockChild(n *html.Node) bool {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.ElementNode && blockChildTags[c.Data] {
			return true
		}
	}
	return false
}

func figureParent(n *html.Node) bool {
	p := n.Parent
	return p != nil && p.Type == html.ElementNode && (p.Data == "figure" || p.Data == "picture")
}

func unwrapNode(n *html.Node) {
	parent := n.Parent
	if parent == nil {
		return
	}
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		n.RemoveChild(c)
		parent.InsertBefore(c, n)
		c = next
	}
	parent.RemoveChild(n)
}

func isPlaceholderImage(n *html.Node) bool {
	for _, a := range n.Attr {
		if a.Key == "aria-label" && strings.EqualFold(a.Val, "image unavailable") {
			return true
		}
	}
	return false
}

func stripDataAttrs(attrs []html.Attribute) []html.Attribute {
	out := attrs[:0]
	for _, a := range attrs {
		if strings.HasPrefix(a.Key, "data-") {
			continue
		}
		out = append(out, a)
	}
	return out
}
