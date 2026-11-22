package domain

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"
)

func publishClip() *Clip {
	return &Clip{
		ID: "c1", Streamer: "alanzoka", Game: "Grand Theft Auto VI",
		Headline: "Ele fugiu da polícia <de moto> e caiu no rio 😂",
		PostText: "Fuga absurda na live! 🎥 Clipe da live de alanzoka na Twitch #GTA6 #alanzoka #twitch",
	}
}

var publishOpts = UploadOptions{
	TitleHashtags: []string{"#GTA6", "#Shorts"},
	Hashtags:      []string{"#gta6", "#GTAVI", "#Shorts", "#Shorts", "sem cerquilha"},
	Tags:          []string{"GTA 6", "GTA VI", "gta 6", "Grand Theft Auto VI"},
	TextLanguage:  "pt-BR", AudioLanguage: "pt",
}

func TestBuildUploadMeta(t *testing.T) {
	m := BuildUploadMeta(publishClip(), publishOpts)
	if want := "Ele fugiu da polícia de moto e caiu no rio 😂 #GTA6 #Shorts"; m.Title != want {
		t.Errorf("title = %q, want %q", m.Title, want)
	}
	want := "Fuga absurda na live! 🎥 Clipe da live de alanzoka na Twitch #GTA6 #alanzoka #twitch\n\n" +
		"🎥 Ao vivo em twitch.tv/alanzoka\n\n#GTAVI #Shorts"
	if m.Description != want {
		t.Errorf("description = %q\nwant %q", m.Description, want)
	}
	wantTags := []string{"GTA 6", "GTA VI", "Grand Theft Auto VI", "alanzoka", "GTA6", "twitch"}
	if !slices.Equal(m.Tags, wantTags) {
		t.Errorf("tags = %q, want %q", m.Tags, wantTags)
	}
	if m.TextLanguage != "pt-BR" || m.AudioLanguage != "pt" {
		t.Errorf("languages = %q / %q", m.TextLanguage, m.AudioLanguage)
	}
}

func TestBuildUploadMetaLimits(t *testing.T) {
	c := publishClip()
	c.Headline = strings.Repeat("palavra ", 20) // 160 characters
	c.PostText = strings.Repeat("ação ", 1200) + "twitch.tv/alanzoka"
	var tags []string
	for i := range 80 {
		tags = append(tags, fmt.Sprintf("tag com espaço %d", i)) // 80 distinct tags, far above the budget
	}
	m := BuildUploadMeta(c, UploadOptions{TitleHashtags: []string{"#GTA6"}, Hashtags: []string{"#GTA6"}, Tags: tags})
	if n := utf8.RuneCountInString(m.Title); n > MaxTitleChars || strings.Contains(m.Title, "#GTA6") {
		t.Errorf("title has %d characters (hashtag only while it fits): %q", n, m.Title)
	}
	if len(m.Description) > MaxDescriptionBytes || !utf8.ValidString(m.Description) {
		t.Errorf("description has %d bytes (valid UTF-8: %v)", len(m.Description), utf8.ValidString(m.Description))
	}
	total := len(m.Tags) - 1 // commas
	for _, tag := range m.Tags {
		total += utf8.RuneCountInString(tag)
		if strings.Contains(tag, " ") {
			total += 2
		}
	}
	if total > MaxTagsChars || len(m.Tags) == 0 {
		t.Errorf("tags use %d characters: %q", total, m.Tags)
	}
}

func TestBuildUploadMetaFallbacks(t *testing.T) {
	c := &Clip{Streamer: "dilera", Headline: "  <>  ", PostText: ""}
	m := BuildUploadMeta(c, UploadOptions{Tags: []string{"<GTA, 6>", `"x"`}})
	if m.Title != "Clipe da live de dilera" {
		t.Errorf("title = %q", m.Title)
	}
	if m.Description != "🎥 Ao vivo em twitch.tv/dilera" {
		t.Errorf("description = %q", m.Description)
	}
	if !slices.Equal(m.Tags, []string{"GTA 6", "x", "dilera"}) {
		t.Errorf("tags = %q", m.Tags)
	}
}

func TestCleanHashtag(t *testing.T) {
	cases := map[string]string{"#GTA6": "#GTA6", "#gta6!": "#gta6", "#": "", "GTA6": "", "#a#b": "", " #x, ": "#x"}
	for in, want := range cases {
		if got := cleanHashtag(in); got != want {
			t.Errorf("cleanHashtag(%q) = %q, want %q", in, got, want)
		}
	}
}
