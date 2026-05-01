package graph

import "testing"

func TestValidGraphID(t *testing.T) {
	cases := []struct {
		id   string
		want bool
		desc string
	}{
		// Real Graph conversation IDs from the Dispatch cache.
		{"AAQkADAwMDAwMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAwMDAwMAAQAAAAAAAAAAAAAAAAAAAAAAA=", true, "real conversation id"},
		{"AQMkADAwMDAwMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAwMDAwMABGAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", true, "real message id"},
		{"AQHczoQd3CH8CBkeBU2UyPI2IADw5g==", true, "with padding"},
		{"abc-123_xyz", true, "minimal alnum + safe chars"},

		// OData injection attempts — must reject.
		{"' or 1=1 --", false, "single quote + space"},
		{"foo' or '1'='1", false, "classic OData injection"},
		{"foo\"bar", false, "double quote"},
		{"foo bar", false, "space"},
		{"foo;DROP", false, "semicolon"},
		{"foo)or(1=1", false, "parens"},
		{"foo,bar", false, "comma"},

		// Edges.
		{"", false, "empty"},
		{"a", true, "single char"},
	}
	for _, c := range cases {
		got := validGraphID(c.id)
		if got != c.want {
			t.Errorf("%s: validGraphID(%q) = %v, want %v", c.desc, c.id, got, c.want)
		}
	}
}
