package models

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A channel's filter is checked against EventTypes, so a type the code records
// but the catalog lacks would be refused to anyone who asks for it. Every
// audited action, every event recorded by name and every Event constant is in
// the catalog.
func TestEveryEmittedEventIsInTheCatalog(t *testing.T) {
	files, _ := filepath.Glob("../*/*.go")
	cmds, _ := filepath.Glob("../../cmd/*/*.go")
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`\.audit\(.*?, "([a-z0-9.-]+)"`),
		regexp.MustCompile(`\.emit\(.*?, "([a-z0-9.-]+)"`),
		regexp.MustCompile(`saveImport\(w, r, [^,]+, "([a-z0-9.-]+)"`),
		regexp.MustCompile(`\bEvent[A-Z]\w*\s*=\s*"([a-z0-9.-]+)"`),
	}
	found := 0
	for _, f := range append(files, cmds...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, re := range patterns {
			for _, m := range re.FindAllStringSubmatch(string(src), -1) {
				found++
				if !KnownEventType(m[1]) {
					t.Errorf("%s records %q, which EventTypes does not list: add it there", f, m[1])
				}
			}
		}
	}
	if found < 60 {
		t.Errorf("found %d event types in the code, fewer than there are: the patterns have stopped matching", found)
	}
}
