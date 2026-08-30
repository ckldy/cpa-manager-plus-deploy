package main

import (
	"strings"
	"testing"
)

func TestStatusPageFollowsSystemColorScheme(t *testing.T) {
	body := string(renderBAIStatusPage(baiStatusPage{}))
	for _, want := range []string{
		"color-scheme:light dark",
		"@media (prefers-color-scheme:dark)",
		"--bg:#f5f7fa",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("status page CSS missing %q", want)
		}
	}
	if strings.Contains(body, "color-scheme:dark;") {
		t.Fatal("status page still forces dark mode")
	}
}
