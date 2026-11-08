package publish

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLogReason(t *testing.T) {
	long := strings.Repeat("a", MaxLogReasonBytes)
	// One byte short of the limit, so the next rune straddles it.
	straddle := strings.Repeat("a", MaxLogReasonBytes-1) + "é"
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"a reason", `"author outside the list"`, "author outside the list"},
		{"no reason", ``, ""},
		{"null", `null`, ""},
		{"blank", `"  \n "`, ""},
		{"a newline cannot start a line", `"a\nlevel=ERROR msg=forged"`, "a level=ERROR msg=forged"},
		{"control and format characters", "\"a\\u001b[0mb\\u202ec\"", "a [0mb c"},
		{"not a string is kept as written", `["a", 1]`, `["a", 1]`},
		{"exactly the limit", `"` + long + `"`, long},
		{"over the limit", `"` + long + `b"`, long + " [truncated]"},
		{"cut on a rune boundary", `"` + straddle + `"`, strings.Repeat("a", MaxLogReasonBytes-1) + " [truncated]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := logReason(json.RawMessage(c.raw))
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Errorf("%q is not valid UTF-8", got)
			}
		})
	}
}
