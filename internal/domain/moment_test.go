package domain

import (
	"slices"
	"strings"
	"testing"
)

func TestCleanClipTitle(t *testing.T) {
	const stream = "🟢 GTA 6 - LIVE  DOS APRECIADORES 🟢"
	cases := []struct{ in, want string }{
		{"  ele   caiu do   prédio KKKK ", "ele caiu do prédio KKKK"},
		{"", ""},
		{"   ", ""},
		{"🟢 gta 6 - live dos apreciadores 🟢", ""}, // Twitch default: the stream title
		{strings.Repeat("á", 120), strings.Repeat("á", 100) + "…"},
	}
	for _, c := range cases {
		if got := CleanClipTitle(c.in, stream); got != c.want {
			t.Errorf("CleanClipTitle(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestMergeClipTitles(t *testing.T) {
	got := MergeClipTitles([]string{"Fuga épica"}, "fuga ÉPICA", "", "Caiu do prédio", "a", "b", "c", "d")
	want := []string{"Fuga épica", "Caiu do prédio", "a", "b", "c"}
	if !slices.Equal(got, want) {
		t.Fatalf("merge = %q, want %q (dedupe ignoring case, skip empty, cap %d)", got, want, MaxClipTitles)
	}
	if got := MergeClipTitles(nil); got != nil {
		t.Fatalf("merge of nothing = %q", got)
	}
}
