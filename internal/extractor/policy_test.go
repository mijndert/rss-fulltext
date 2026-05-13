package extractor

import (
	"strings"
	"testing"
)

func TestDefaultPolicyKeepsAltWithColons(t *testing.T) {
	p := defaultPolicy()
	in := `<img src="https://example.com/x.png" alt="Architecture overview: a single NUC running services, with hairpin NAT."/>`
	out := p.Sanitize(in)
	if !strings.Contains(out, `alt="Architecture overview: a single NUC running services, with hairpin NAT."`) {
		t.Errorf("alt with colon was stripped: %q", out)
	}
}

func TestDefaultPolicyKeepsSrcsetWithHTTPS(t *testing.T) {
	p := defaultPolicy()
	in := `<img src="https://example.com/x.png" srcset="https://example.com/x.png 400w, https://example.com/x@2x.png 800w"/>`
	out := p.Sanitize(in)
	if !strings.Contains(out, `srcset=`) {
		t.Errorf("srcset was stripped: %q", out)
	}
}

func TestDefaultPolicyRejectsJavascriptSrcset(t *testing.T) {
	p := defaultPolicy()
	in := `<img src="https://example.com/x.png" srcset="javascript:alert(1) 1x"/>`
	out := p.Sanitize(in)
	if strings.Contains(out, "javascript:") {
		t.Errorf("javascript: srcset was not stripped: %q", out)
	}
}

func TestDefaultPolicyKeepsLoadingDecoding(t *testing.T) {
	p := defaultPolicy()
	in := `<img src="https://example.com/x.png" loading="lazy" decoding="async"/>`
	out := p.Sanitize(in)
	if !strings.Contains(out, `loading="lazy"`) || !strings.Contains(out, `decoding="async"`) {
		t.Errorf("loading/decoding stripped: %q", out)
	}
}

func TestDefaultPolicyRejectsInvalidLoading(t *testing.T) {
	p := defaultPolicy()
	in := `<img src="https://example.com/x.png" loading="invalid-value"/>`
	out := p.Sanitize(in)
	if strings.Contains(out, "loading=") {
		t.Errorf("invalid loading value was allowed: %q", out)
	}
}

func TestDefaultPolicyStillStripsScripts(t *testing.T) {
	p := defaultPolicy()
	in := `<p>Hi</p><script>alert(1)</script>`
	out := p.Sanitize(in)
	if strings.Contains(out, "<script>") || strings.Contains(out, "alert") {
		t.Errorf("scripts were not stripped: %q", out)
	}
}
