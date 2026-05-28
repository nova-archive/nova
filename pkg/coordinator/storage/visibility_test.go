package storage

import "testing"

func TestResolveVisibility(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want Visibility
	}{
		{"none", nil, VisibilityPrivate},
		{"only private memberships", []string{"private", "private"}, VisibilityPrivate},
		{"unlisted upgrades", []string{"private", "unlisted"}, VisibilityUnlisted},
		{"public wins", []string{"unlisted", "public", "private"}, VisibilityPublic},
		{"single public", []string{"public"}, VisibilityPublic},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveVisibility(c.in); got != c.want {
				t.Fatalf("resolveVisibility(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}
