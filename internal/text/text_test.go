package text

import "testing"

func TestPlain(t *testing.T) {
	for _, s := range []string{"", "Sam", "Zoë", "Danny's friend", "ゲーム", "🎮 night"} {
		if !Plain(s) {
			t.Errorf("%q refused", s)
		}
	}
	for _, s := range []string{"two\nlines", "tab\there", "nul\x00", "del\x7f", "turn\u202eed", "zero\u200bwidth", "joined\u200dup", "bom\ufeff", "\xff"} {
		if Plain(s) {
			t.Errorf("%q allowed", s)
		}
	}
}
