package textutil

import (
	"html"
	"regexp"
	"strings"
)

var (
	reBR     = regexp.MustCompile(`(?i)<br\s*/?\s*>`)
	reBlock  = regexp.MustCompile(`(?i)</(p|div|li|h[1-6]|tr)>`)
	reTags   = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces = regexp.MustCompile(`[ \t\xA0]+`)
	reBlank  = regexp.MustCompile(`\n{3,}`)
)

// FromHTML converts Google Calendar HTML descriptions into plain text for Todomate memos.
func FromHTML(s string) string {
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "<") {
		return strings.TrimSpace(s)
	}
	out := reBR.ReplaceAllString(s, "\n")
	out = reBlock.ReplaceAllString(out, "\n")
	out = reTags.ReplaceAllString(out, "")
	out = html.UnescapeString(out)
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "\n")
	lines := strings.Split(out, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimSpace(reSpaces.ReplaceAllString(line, " "))
	}
	out = strings.Join(lines, "\n")
	out = reBlank.ReplaceAllString(out, "\n\n")
	return strings.TrimSpace(out)
}

// LooksLikeHTML reports whether s likely contains HTML markup.
func LooksLikeHTML(s string) bool {
	return strings.Contains(s, "<") && strings.Contains(s, ">")
}
