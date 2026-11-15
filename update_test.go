package rss

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// rss2XML builds a minimal RSS 2.0 feed with one item
// per given ID.
func rss2XML(title string, ids ...string) string {
	var b strings.Builder
	b.WriteString(`<rss version="2.0"><channel>`)
	fmt.Fprintf(&b, `<title>%s</title>`, title)
	for _, id := range ids {
		fmt.Fprintf(&b, `<item><guid>%s</guid><title>Item %s</title></item>`, id, id)
	}
	b.WriteString(`</channel></rss>`)
	return b.String()
}

// itemIDs returns the IDs of the feed's items, in order.
func itemIDs(feed *Feed) []string {
	ids := make([]string, len(feed.Items))
	for i, item := range feed.Items {
		ids[i] = item.ID
	}
	return ids
}

// equalStrings reports whether two string slices are equal.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// feedServer is a test feed server with programmable
// responses and support for conditional requests.
type feedServer struct {
	t *testing.T

	body         atomic.Value // string
	etag         string
	lastModified string
	etagCounter  int

	lastIfNoneMatch     atomic.Value // string
	lastIfModifiedSince atomic.Value // string
}

func newFeedServer(t *testing.T, body string) (*feedServer, *httptest.Server) {
	t.Helper()

	f := &feedServer{t: t, etag: `"v1"`, lastModified: "Mon, 03 Nov 2025 12:00:00 GMT"}
	f.body.Store(body)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.lastIfNoneMatch.Store(req.Header.Get("If-None-Match"))
		f.lastIfModifiedSince.Store(req.Header.Get("If-Modified-Since"))

		if f.etag != "" {
			w.Header().Set("ETag", f.etag)
		}
		if f.lastModified != "" {
			w.Header().Set("Last-Modified", f.lastModified)
		}

		if f.etag != "" && req.Header.Get("If-None-Match") == f.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		if f.lastModified != "" && req.Header.Get("If-Modified-Since") == f.lastModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}

		fmt.Fprint(w, f.body.Load().(string))
	}))

	t.Cleanup(srv.Close)
	return f, srv
}

// setBody changes the body served by the feed. Like a
// real server, the validators change with the content.
func (f *feedServer) setBody(body string) {
	f.body.Store(body)
	f.etagCounter++
	if f.etag != "" {
		f.etag = fmt.Sprintf(`"v%d"`, f.etagCounter+1)
	}
	if f.lastModified != "" {
		f.lastModified = fmt.Sprintf("Mon, 03 Nov 2025 12:%02d:00 GMT", f.etagCounter)
	}
}

// TestUpdateConvergesWithFetch checks that a sequence of
// incremental updates results in the same items, in the
// same order, as a fresh fetch of the same feed, while
// preserving the read state of surviving items.
func TestUpdateConvergesWithFetch(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	// The feed starts with items a, b, c.
	fs, srv := newFeedServer(t, rss2XML("Test feed", "a", "b", "c"))

	feed, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}

	// Mark b as read.
	feed.Items[1].Read = true
	feed.Unread = 2

	// The publisher reorders the feed, removes b, and
	// adds d: the new order is c, d, a.
	fs.setBody(rss2XML("Test feed", "c", "d", "a"))

	// Force the update to be due.
	feed.NextUpdate = time.Now().Add(-time.Hour)
	if err := r.Update(ctx, feed); err != nil {
		t.Fatalf("failed to update feed: %v", err)
	}

	// A fresh fetch of the same feed must give the same
	// set of items in the same order.
	fresh, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}

	if got, want := itemIDs(feed), itemIDs(fresh); !equalStrings(got, want) {
		t.Fatalf("updated feed differs from fresh fetch:\ngot:  %v\nwant: %v", got, want)
	}

	want := []string{"c", "d", "a"}
	if got := itemIDs(feed); !equalStrings(got, want) {
		t.Fatalf("unexpected items after update: got %v, want %v", got, want)
	}

	// b has been removed; c and a remain unread; d is
	// new and unread.
	if feed.Unread != 3 {
		t.Fatalf("unexpected unread count: got %d, want 3", feed.Unread)
	}
	for _, item := range feed.Items {
		if item.Read {
			t.Fatalf("item %q unexpectedly marked read", item.ID)
		}
	}
}

// TestUpdatePreservesRead checks that the read state of
// items that survive an update is preserved.
func TestUpdatePreservesRead(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	fs, srv := newFeedServer(t, rss2XML("Test feed", "a", "b", "c"))

	feed, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}

	// Mark a and c as read.
	feed.Items[0].Read = true
	feed.Items[2].Read = true
	feed.Unread = 1

	// The publisher reorders the surviving items and
	// adds d.
	fs.setBody(rss2XML("Test feed", "c", "d", "a"))

	feed.NextUpdate = time.Now().Add(-time.Hour)
	if err := r.Update(ctx, feed); err != nil {
		t.Fatalf("failed to update feed: %v", err)
	}

	read := make(map[string]bool)
	for _, item := range feed.Items {
		read[item.ID] = item.Read
	}

	for _, id := range []string{"a", "c"} {
		if !read[id] {
			t.Fatalf("item %q lost its read state", id)
		}
	}
	if read["d"] {
		t.Fatalf("new item %q unexpectedly marked read", "d")
	}
	if feed.Unread != 1 {
		t.Fatalf("unexpected unread count: got %d, want 1", feed.Unread)
	}
}

// TestUpdateNotModified checks that updates reuse the
// ETag and Last-Modified validators from the previous
// fetch, and that a 304 response leaves the feed's
// items untouched.
func TestUpdateNotModified(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	fs, srv := newFeedServer(t, rss2XML("Test feed", "a", "b"))

	feed, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}

	before := time.Now()
	feed.NextUpdate = before.Add(-time.Hour)
	if err := r.Update(ctx, feed); err != nil {
		t.Fatalf("failed to update feed: %v", err)
	}

	// The update must have made a conditional request
	// using the validators from the first response.
	if got, want := fs.lastIfNoneMatch.Load().(string), fs.etag; got != want {
		t.Fatalf("unexpected If-None-Match header: got %q, want %q", got, want)
	}
	if got, want := fs.lastIfModifiedSince.Load().(string), fs.lastModified; got != want {
		t.Fatalf("unexpected If-Modified-Since header: got %q, want %q", got, want)
	}

	// The 304 response must not have changed the items.
	if got, want := itemIDs(feed), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("304 response changed items: got %v, want %v", got, want)
	}
	if feed.Unread != 2 {
		t.Fatalf("304 response changed unread count: got %d, want 2", feed.Unread)
	}

	// The next update should have been pushed back.
	if !feed.NextUpdate.After(before) {
		t.Fatalf("304 response did not push back NextUpdate: got %v", feed.NextUpdate)
	}
}

// TestUpdateGone checks that a 410 response is reported
// accurately and leaves the previous snapshot unchanged.
func TestUpdateGone(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	fs, srv := newFeedServer(t, rss2XML("Test feed", "a", "b"))

	feed, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}
	feed.Items[0].Read = true
	feed.Unread = 1

	// The feed is removed.
	fs.etag = ""
	fs.lastModified = ""
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusGone)
	})

	feed.NextUpdate = time.Now().Add(-time.Hour)
	err = r.Update(ctx, feed)
	if err == nil {
		t.Fatal("expected error when feed is gone, got nil")
	}

	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected a StatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusGone {
		t.Fatalf("unexpected status code: got %d, want %d", statusErr.StatusCode, http.StatusGone)
	}

	// The old snapshot must be unchanged.
	if got, want := itemIDs(feed), []string{"a", "b"}; !equalStrings(got, want) {
		t.Fatalf("410 response changed items: got %v, want %v", got, want)
	}
	if !feed.Items[0].Read || feed.Unread != 1 {
		t.Fatalf("410 response changed read state: %+v", feed.Items)
	}
}

// TestUpdateFailurePreservesSnapshot checks that network
// and parse failures leave the previous snapshot unchanged.
func TestUpdateFailurePreservesSnapshot(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	t.Run("network error", func(t *testing.T) {
		fs, srv := newFeedServer(t, rss2XML("Test feed", "a", "b"))

		feed, err := r.Fetch(ctx, srv.URL)
		if err != nil {
			t.Fatalf("failed to fetch feed: %v", err)
		}

		// Kill the server.
		srv.Close()

		feed.NextUpdate = time.Now().Add(-time.Hour)
		if err := r.Update(ctx, feed); err == nil {
			t.Fatal("expected error when server is unreachable, got nil")
		}

		if got, want := itemIDs(feed), []string{"a", "b"}; !equalStrings(got, want) {
			t.Fatalf("network error changed items: got %v, want %v", got, want)
		}
		if feed.Unread != 2 {
			t.Fatalf("network error changed unread count: got %d, want 2", feed.Unread)
		}

		_ = fs
	})

	t.Run("parse error", func(t *testing.T) {
		fs, srv := newFeedServer(t, rss2XML("Test feed", "a", "b"))

		feed, err := r.Fetch(ctx, srv.URL)
		if err != nil {
			t.Fatalf("failed to fetch feed: %v", err)
		}

		// Serve invalid XML. Drop the validators so the
		// update isn't answered with a 304.
		fs.etag = ""
		fs.lastModified = ""
		fs.setBody("<rss version=\"2.0\"><channel><title>broken")

		feed.NextUpdate = time.Now().Add(-time.Hour)
		if err := r.Update(ctx, feed); err == nil {
			t.Fatal("expected error when feed is malformed, got nil")
		}

		if got, want := itemIDs(feed), []string{"a", "b"}; !equalStrings(got, want) {
			t.Fatalf("parse error changed items: got %v, want %v", got, want)
		}
		if feed.Unread != 2 {
			t.Fatalf("parse error changed unread count: got %d, want 2", feed.Unread)
		}
	})
}

// TestUpdateDuplicateIDs checks that an update containing
// duplicate item IDs converges on the same deduplicated
// set of items as a fresh fetch.
func TestUpdateDuplicateIDs(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	fs, srv := newFeedServer(t, rss2XML("Test feed", "a"))

	feed, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}

	// The publisher adds b, but accidentally includes a
	// twice.
	fs.setBody(rss2XML("Test feed", "b", "a", "a"))

	feed.NextUpdate = time.Now().Add(-time.Hour)
	if err := r.Update(ctx, feed); err != nil {
		t.Fatalf("failed to update feed: %v", err)
	}

	fresh, err := r.Fetch(ctx, srv.URL)
	if err != nil {
		t.Fatalf("failed to fetch feed: %v", err)
	}

	if got, want := itemIDs(feed), itemIDs(fresh); !equalStrings(got, want) {
		t.Fatalf("updated feed differs from fresh fetch:\ngot:  %v\nwant: %v", got, want)
	}

	want := []string{"b", "a"}
	if got := itemIDs(feed); !equalStrings(got, want) {
		t.Fatalf("unexpected items after update: got %v, want %v", got, want)
	}
	if feed.Unread != 2 {
		t.Fatalf("unexpected unread count: got %d, want 2", feed.Unread)
	}
}

// TestFetchStatusError checks that Fetch reports non-2xx
// responses accurately.
func TestFetchStatusError(t *testing.T) {
	ctx := context.Background()

	r, err := NewReader()
	if err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(http.StatusGone)
	}))
	defer srv.Close()

	_, err = r.Fetch(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error when feed is gone, got nil")
	}

	var statusErr *StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("expected a StatusError, got %T: %v", err, err)
	}
	if statusErr.StatusCode != http.StatusGone {
		t.Fatalf("unexpected status code: got %d, want %d", statusErr.StatusCode, http.StatusGone)
	}
}
