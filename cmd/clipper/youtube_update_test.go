package main

import (
	"testing"

	"github.com/henriquecpp/gta6-clipper/internal/adapters/out/youtube"
	"github.com/henriquecpp/gta6-clipper/internal/domain"
)

func TestTextChanged(t *testing.T) {
	cur := youtube.Snippet{Title: "t", Description: "d", Tags: []string{"a", "b"}, CategoryID: "20"}
	meta := domain.UploadMeta{Title: "t", Description: "d", Tags: []string{"a", "b"}, TextLanguage: "pt-BR"}
	if textChanged(cur, meta) {
		t.Error("same texts reported as changed")
	}
	// YouTube returns the tags in its own order (and may change the case).
	if textChanged(cur, domain.UploadMeta{Title: "t", Description: "d", Tags: []string{"B", "a"}}) {
		t.Error("reordered tags reported as changed")
	}
	if added, removed := tagsDiff([]string{"a", "b"}, []string{"b", "c"}); len(added) != 1 || added[0] != "c" || len(removed) != 1 || removed[0] != "a" {
		t.Errorf("tagsDiff = %q, %q", added, removed)
	}
	for name, m := range map[string]domain.UploadMeta{
		"title":       {Title: "t2", Description: "d", Tags: []string{"a", "b"}},
		"description": {Title: "t", Description: "d\n\n#GTA6", Tags: []string{"a", "b"}},
		"tags":        {Title: "t", Description: "d", Tags: []string{"a", "b", "c"}},
	} {
		if !textChanged(cur, m) {
			t.Errorf("%s change not detected", name)
		}
	}
}
