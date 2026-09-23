package sessions

import "testing"

func TestSanitizeNameStripsLeadingDash(t *testing.T) {
	cases := map[string]string{
		"-foo":    "foo",
		"--bar":   "bar",
		"-":       "",
		"--":      "",
		"ok-name": "ok-name",
		"  -x  ":  "x",
		"":        "",
	}
	for in, want := range cases {
		if got := sanitizeName(in); got != want {
			t.Errorf("sanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
