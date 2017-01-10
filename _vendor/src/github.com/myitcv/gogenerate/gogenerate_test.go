// Copyright (c) 2016 Paul Jolly <paul@myitcv.org.uk>, all rights reserved.
// Use of this document is governed by a license found in the LICENSE document.

package gogenerate

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestCommentRegexp(t *testing.T) {
	comm := "banana"
	r, err := commentRegex(comm)
	if err != nil {
		t.Fatalf("Expected call to result in no error")
	}

	checks := []struct {
		s string
		r bool
	}{
		{GoGeneratePrefix + " " + comm, true},
		{GoGeneratePrefix + " " + comm + " ", true},
		{GoGeneratePrefix + " " + comm + "  some arguments\n", true},
		{GoGeneratePrefix + " " + comm + "\t", false},
		{GoGeneratePrefix + " " + comm + "\t", false},
	}

	for _, c := range checks {
		v := r.MatchString(c.s)
		if v != c.r {
			t.Errorf("commentRegex /%v/.MatchString(%q) does not equal %v", r, c.s, c.r)
		}
	}
}

func TestFilesContaining(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Error(err)
	}

	checks := []struct {
		d       string
		cmds    []string
		matches []string
	}{
		{"_testFiles/eg01", []string{"ls", "/bin/ls"}, []string{"a.go", "b.go", "c.go", "d.go"}},
	}

Checks:
	for _, c := range checks {

		path := filepath.Join(cwd, c.d)

		res, err := FilesContainingCmd(path, c.cmds...)
		if err != nil {
			t.Errorf("Got unexpected error find matches in %v: %v", c.d, err)
			continue Checks
		}

		if len(res) != len(c.matches) {
			t.Errorf("Matches not up to expectations: %v vs %v", res, c.matches)
			continue Checks
		}

		// just in case we were sloppy in the test table
		sort.Sort(byBase(c.matches))
		for i := range res {
			if res[i] != c.matches[i] {
				t.Errorf("Matches not up to expectations: %v vs %v", res, c.matches)
				continue Checks
			}
		}
	}
}
