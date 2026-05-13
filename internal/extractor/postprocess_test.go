package extractor

import (
	"strings"
	"testing"

	"golang.org/x/net/html"
)

func elem(name string, children ...*html.Node) *html.Node {
	n := &html.Node{Type: html.ElementNode, Data: name}
	for _, c := range children {
		n.AppendChild(c)
	}
	return n
}

func attrElem(name string, attrs map[string]string, children ...*html.Node) *html.Node {
	n := elem(name, children...)
	for k, v := range attrs {
		n.Attr = append(n.Attr, html.Attribute{Key: k, Val: v})
	}
	return n
}

func text(s string) *html.Node { return &html.Node{Type: html.TextNode, Data: s} }

func render(t *testing.T, root *html.Node) string {
	t.Helper()
	var b strings.Builder
	for c := root.FirstChild; c != nil; c = c.NextSibling {
		if err := html.Render(&b, c); err != nil {
			t.Fatalf("render: %v", err)
		}
	}
	return b.String()
}

func simplify(t *testing.T, root *html.Node) string {
	t.Helper()
	simplifyDOM(root)
	return render(t, root)
}

func TestSimplifyUnwrapsParagraphAroundHeading(t *testing.T) {
	// Real readability output sometimes wraps an <h2> in a <p>, which is
	// invalid HTML — the parser auto-closes it back open, leaving orphan tags
	// in feed readers.
	root := elem("body",
		elem("p", elem("h2", text("Title"))),
	)
	got := simplify(t, root)
	if got != `<h2>Title</h2>` {
		t.Errorf("got %q", got)
	}
}

func TestSimplifyUnwrapsAttributelessDiv(t *testing.T) {
	root := elem("body",
		attrElem("div", map[string]string{"data-component": "text-block"},
			elem("p", text("Hello world")),
		),
	)
	got := simplify(t, root)
	if got != `<p>Hello world</p>` {
		t.Errorf("got %q", got)
	}
}

func TestSimplifyKeepsDivWithClassOrID(t *testing.T) {
	root := elem("body",
		attrElem("div", map[string]string{"id": "readability-page-1", "class": "page"},
			elem("p", text("Hello")),
		),
	)
	got := simplify(t, root)
	if !strings.Contains(got, `id="readability-page-1"`) || !strings.Contains(got, `<p>Hello</p>`) {
		t.Errorf("expected outer div to survive, got %q", got)
	}
}

func TestSimplifyUnwrapsParagraphInsideFigureCascades(t *testing.T) {
	// BBC pattern: figure > div(no-attrs) > p > img + span(no-attrs).
	// The <div> unwraps first, exposing the <p> as direct child of <figure>,
	// which then unwraps on a second pass.
	root := elem("body",
		elem("figure",
			elem("div",
				elem("p",
					attrElem("img", map[string]string{"src": "x.jpg"}),
					elem("span", text("Caption")),
				),
			),
		),
	)
	got := simplify(t, root)
	want := `<figure><img src="x.jpg"/>Caption</figure>`
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestSimplifyLeavesNormalParagraphAlone(t *testing.T) {
	root := elem("body",
		elem("p",
			text("Inline text with "),
			attrElem("img", map[string]string{"src": "x.jpg"}),
			text(" an image."),
		),
	)
	got := simplify(t, root)
	if got != `<p>Inline text with <img src="x.jpg"/> an image.</p>` {
		t.Errorf("got %q", got)
	}
}

func TestSimplifyStripsDataAttributes(t *testing.T) {
	root := elem("body",
		attrElem("article", map[string]string{"data-foo": "bar", "id": "keep"},
			elem("p", text("x")),
		),
	)
	got := simplify(t, root)
	if strings.Contains(got, "data-foo") {
		t.Errorf("expected data-foo to be stripped, got %q", got)
	}
	if !strings.Contains(got, `id="keep"`) {
		t.Errorf("expected id to survive, got %q", got)
	}
}

func TestSimplifyDropsPlaceholderImage(t *testing.T) {
	root := elem("body",
		elem("figure",
			attrElem("img", map[string]string{"src": "grey-placeholder.png", "aria-label": "image unavailable"}),
			attrElem("img", map[string]string{"src": "real.jpg", "alt": "Real photo"}),
		),
	)
	got := simplify(t, root)
	if !strings.Contains(got, "<figure>") || !strings.Contains(got, "</figure>") {
		t.Errorf("expected figure wrapper preserved, got %q", got)
	}
	if !strings.Contains(got, `src="real.jpg"`) || !strings.Contains(got, `alt="Real photo"`) {
		t.Errorf("real image attrs missing, got %q", got)
	}
	if strings.Contains(got, "grey-placeholder") || strings.Contains(got, "image unavailable") {
		t.Errorf("placeholder image should have been dropped, got %q", got)
	}
}

func TestSimplifyKeepsImageWithoutPlaceholderLabel(t *testing.T) {
	root := elem("body",
		attrElem("img", map[string]string{"src": "real.jpg", "aria-label": "A real picture"}),
	)
	got := simplify(t, root)
	if !strings.Contains(got, `src="real.jpg"`) {
		t.Errorf("expected image to survive, got %q", got)
	}
}

func TestSimplifyUnwrapsSpanWithOnlyDataAttrs(t *testing.T) {
	root := elem("body",
		elem("p",
			text("before "),
			attrElem("span", map[string]string{"data-testid": "x"}, text("middle")),
			text(" after"),
		),
	)
	got := simplify(t, root)
	if got != `<p>before middle after</p>` {
		t.Errorf("got %q", got)
	}
}
