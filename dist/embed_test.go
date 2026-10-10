package web

import (
	"regexp"
	"testing"
)

func TestPagesEmbedReferencedAssets(t *testing.T) {
	references := regexp.MustCompile(`(?:src|href)="/([^"?]+\.(?:js|css))"`)
	for _, page := range []string{"index.html", "status.html"} {
		body, err := Assets.ReadFile(page)
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range references.FindAllSubmatch(body, -1) {
			if _, err := Assets.ReadFile(string(match[1])); err != nil {
				t.Errorf("%s references an asset missing from the binary: %s: %v", page, match[1], err)
			}
		}
	}
}
