package domain

import (
	"strings"
	"unicode/utf8"
)

// Publication is where a final video was published.
type Publication struct {
	// ID is the id of the video on the platform (YouTube video id).
	ID  string
	URL string
}

// UploadMeta is the text published with a Short.
type UploadMeta struct {
	Title       string
	Description string
	Tags        []string
	// TextLanguage is the language of the title and description;
	// AudioLanguage is the language spoken in the video.
	TextLanguage  string
	AudioLanguage string
}

// UploadOptions configures BuildUploadMeta (from the configuration). In
// TitleHashtags, DescriptionLine, Hashtags and Tags, "{streamer}" is replaced
// by the login.
type UploadOptions struct {
	// TitleHashtags are appended to the title while it fits.
	TitleHashtags []string
	// DescriptionLine follows the post text in the description: the names
	// people search for (GTA 6 and the full name of the game) and the channel
	// link. When it lacks the link, the link gets its own line.
	DescriptionLine string
	// Hashtags are appended to the description when the post text lacks them.
	Hashtags []string
	// Tags are the base tags, most important first. The editor's search tags
	// of the moment come before them; the streamer, the game and the hashtags
	// of the post text after them, all within the 500 character budget.
	Tags          []string
	TextLanguage  string
	AudioLanguage string
}

// StreamerPlaceholder is replaced by the streamer login in the upload options.
const StreamerPlaceholder = "{streamer}"

// YouTube limits (developers.google.com/youtube/v3/docs/videos: title up to
// 100 characters, description up to 5000 bytes, tags up to 500 characters in
// total, both without < and >).
const (
	MaxTitleChars       = 100
	MaxDescriptionBytes = 5000
	MaxTagsChars        = 500
)

// BuildUploadMeta writes the title, description and tags of a clip's Short:
// the editor's YouTube title (or the headline) as the title, the post text
// (which credits the streamer) plus the description line, the channel link
// and the configured hashtags as the description, and the moment's search
// tags, the configured tags, the streamer, the game and the post hashtags as
// tags.
func BuildUploadMeta(c *Clip, o UploadOptions) UploadMeta {
	expand := func(list []string) []string {
		out := make([]string, len(list))
		for i, s := range list {
			out[i] = strings.ReplaceAll(s, StreamerPlaceholder, c.Streamer)
		}
		return out
	}
	var search []string
	if c.Plan != nil {
		search = c.Plan.SearchTags
	}
	return UploadMeta{
		Title:         uploadTitle(c, expand(o.TitleHashtags)),
		Description:   uploadDescription(c, strings.ReplaceAll(o.DescriptionLine, StreamerPlaceholder, c.Streamer), expand(o.Hashtags)),
		Tags:          uploadTags(append(append([]string{}, search...), expand(o.Tags)...), c),
		TextLanguage:  o.TextLanguage,
		AudioLanguage: o.AudioLanguage,
	}
}

func uploadTitle(c *Clip, hashtags []string) string {
	title := ""
	if c.Plan != nil {
		title = strings.Join(strings.Fields(stripAngles(c.Plan.YouTubeTitle)), " ")
	}
	if title == "" {
		title = strings.Join(strings.Fields(stripAngles(c.Headline)), " ")
	}
	if title == "" {
		title = "Clipe da live de " + c.Streamer
	}
	title = cutRunes(title, MaxTitleChars)
	for _, h := range hashtags {
		h = cleanHashtag(h)
		if h == "" || hasToken(title, h) {
			continue
		}
		if utf8.RuneCountInString(title)+1+utf8.RuneCountInString(h) <= MaxTitleChars {
			title += " " + h
		}
	}
	return title
}

func uploadDescription(c *Clip, line string, hashtags []string) string {
	desc := strings.TrimSpace(stripAngles(c.PostText))
	if line = strings.TrimSpace(stripAngles(line)); line != "" && !strings.Contains(desc, line) {
		desc = strings.TrimSpace(desc + "\n\n" + line)
	}
	if link := "twitch.tv/" + c.Streamer; !strings.Contains(strings.ToLower(desc), strings.ToLower(link)) {
		desc = strings.TrimSpace(desc + "\n\n🎥 Ao vivo em " + link)
	}
	var missing []string
	for _, h := range hashtags {
		if h = cleanHashtag(h); h != "" && !hasToken(desc, h) && !hasToken(strings.Join(missing, " "), h) {
			missing = append(missing, h)
		}
	}
	if len(missing) > 0 {
		desc += "\n\n" + strings.Join(missing, " ")
	}
	return cutBytes(desc, MaxDescriptionBytes)
}

// uploadTags keeps the tags in order, without duplicates, within the 500
// character budget, where commas between tags count and a tag with a space
// counts two more characters (the API quotes it).
func uploadTags(base []string, c *Clip) []string {
	cand := append([]string{}, base...)
	cand = append(cand, c.Streamer, c.Game)
	for _, f := range strings.Fields(c.PostText) {
		if h := cleanHashtag(f); h != "" {
			cand = append(cand, strings.TrimPrefix(h, "#"))
		}
	}
	tags := []string{}
	seen := map[string]bool{}
	total := 0
	for _, t := range cand {
		t = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
			if r == '<' || r == '>' || r == ',' || r == '"' {
				return -1
			}
			return r
		}, t)), " ")
		key := strings.ToLower(t)
		if t == "" || seen[key] {
			continue
		}
		cost := utf8.RuneCountInString(t)
		if strings.Contains(t, " ") {
			cost += 2
		}
		if len(tags) > 0 {
			cost++
		}
		if total+cost > MaxTagsChars {
			continue
		}
		seen[key] = true
		total += cost
		tags = append(tags, t)
	}
	return tags
}

// cleanHashtag returns "#word" (trailing punctuation removed) or "" if s is
// not a hashtag.
func cleanHashtag(s string) string {
	s = strings.TrimRight(strings.TrimSpace(s), ".,;:!?)")
	if len(s) < 2 || s[0] != '#' || strings.ContainsAny(s, " <>") || strings.Contains(s[1:], "#") {
		return ""
	}
	return s
}

// hasToken reports whether text contains the hashtag as a whole word,
// ignoring case.
func hasToken(text, tag string) bool {
	for _, f := range strings.Fields(text) {
		if strings.EqualFold(strings.TrimRight(f, ".,;:!?)"), tag) {
			return true
		}
	}
	return false
}

func stripAngles(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '<' || r == '>' {
			return -1
		}
		return r
	}, s)
}

func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n]))
}

// cutBytes cuts s to at most n bytes without splitting a UTF-8 sequence.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return strings.TrimSpace(s[:n])
}
