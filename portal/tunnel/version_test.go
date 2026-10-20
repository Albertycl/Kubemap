package tunnel

import (
	"strings"
	"testing"
)

func TestVersionText(t *testing.T) {
	for in, want := range map[string]string{
		"0.1.3":                 "0.1.3",
		"v1.34.8+k3s1":          "v1.34.8+k3s1",
		"f52b4b7-dirty":         "f52b4b7-dirty",
		"1.0 <script>":          "1.0??script?",
		"":                      "",
		strings.Repeat("a", 90): strings.Repeat("a", maxVersionLen),
	} {
		if got := versionText(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}
