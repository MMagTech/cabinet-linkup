package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"v1.1.0", "1.0.0", true},
		{"1.0.1", "1.0.0", true},
		{"2.0.0", "1.9.9", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.0.1", false},
		{"1.10.0", "1.9.0", true},
		{"nonsense", "1.0.0", false},
		{"1.0.0", "dev", false},
		{"1.0", "1.0.0", false},
		{"1.0.0-rc1", "1.0.0", false},
		{"1.0.0", "testing-1a2b3c4", false},
		{"testing-1a2b3c4", "1.0.0", false},
	} {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v", c.a, c.b, got)
		}
	}
}

func serve(t *testing.T, status int, body string) *Checker {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.ContentLength > 0 {
			t.Error("it must send nothing")
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	c := New("1.0.0")
	c.URL = srv.URL
	return c
}

func TestCheck(t *testing.T) {
	c := serve(t, 200, `{"tag_name":"v1.2.0"}`)
	c.Check(context.Background())
	if c.Latest() != "1.2.0" {
		t.Errorf("latest %q", c.Latest())
	}
}

func TestSameVersionShowsNothing(t *testing.T) {
	c := serve(t, 200, `{"tag_name":"v1.0.0"}`)
	c.Check(context.Background())
	if c.Latest() != "" {
		t.Errorf("latest %q", c.Latest())
	}
}

func TestPrivateRepositoryOrFailureShowsNothing(t *testing.T) {
	for _, c := range []*Checker{serve(t, 404, `{}`), serve(t, 200, `not json`)} {
		c.Check(context.Background())
		if c.Latest() != "" {
			t.Errorf("latest %q", c.Latest())
		}
	}
}
