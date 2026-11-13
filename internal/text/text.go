// Package text is the one check on words that come from outside: from a console, from a linked
// server, or typed on the admin page. Each place keeps its own limits; what counts as a character
// nobody can see is decided here, once.
package text

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Hidden says whether a character is one nobody sees in a line of text: a control character, or an
// invisible formatting one, such as the kind that turns the text after it right to left.
func Hidden(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// Plain says whether s is valid UTF-8 with no hidden character in it.
func Plain(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsFunc(s, Hidden)
}
