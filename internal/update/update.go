// Package update finds out whether a newer server has been released, by asking GitHub,
// for the notice on the admin page. It sends nothing about the server. Updates are never
// forced, and a failure to ask just means no notice.
package update

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ReleasesURL is the address of what it asks.
const ReleasesURL = "https://api.github.com/repos/MMagTech/cabinet-linkup/releases/latest"

// PageURL is where the notice's "What changed" goes.
const PageURL = "https://github.com/MMagTech/cabinet-linkup/releases"

// Checker holds the latest release it has heard of.
type Checker struct {
	Current string
	URL     string
	Client  *http.Client

	mu     sync.Mutex
	latest string
}

// New makes a checker for this server's version.
func New(current string) *Checker {
	return &Checker{Current: current, URL: ReleasesURL, Client: &http.Client{Timeout: 20 * time.Second}}
}

// Latest returns a newer version if there is one, or "".
func (c *Checker) Latest() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Run asks soon after starting, then once a day, until the context ends.
func (c *Checker) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Minute):
	}
	for {
		c.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(24 * time.Hour):
		}
	}
}

// Check asks once.
func (c *Checker) Check(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "cabinet-linkup")
	resp, err := c.Client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	var release struct {
		Tag string `json:"tag_name"`
	}
	if json.NewDecoder(resp.Body).Decode(&release) != nil {
		return
	}
	newer := ""
	if Newer(release.Tag, c.Current) {
		newer = strings.TrimPrefix(release.Tag, "v")
	}
	c.mu.Lock()
	c.latest = newer
	c.mu.Unlock()
}

// Newer says whether version a is after version b. A version that is not plain numbers,
// such as a :testing build's testing-1a2b3c4, is never newer and nothing is newer than it,
// so a :testing server shows no notice: it follows main, which is ahead of every release.
func Newer(a, b string) bool {
	x, ok1 := parts(a)
	y, ok2 := parts(b)
	if !ok1 || !ok2 {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return x[i] > y[i]
		}
	}
	return false
}

func parts(v string) ([3]int, bool) {
	var out [3]int
	fields := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(fields) != 3 {
		return out, false
	}
	for i, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
