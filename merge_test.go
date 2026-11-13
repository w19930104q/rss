package rss

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/SlyMarbo/rss/v2/atom1"
	"github.com/SlyMarbo/rss/v2/rss1"
	"github.com/SlyMarbo/rss/v2/rss2"
)

// feedEntry is a format-agnostic description of a single
// item used to build test feeds and expected results.
type feedEntry struct {
	id      string
	title   string
	summary string
	content string
	link    string
	date    string // RFC1123Z for RSS, RFC3339 for Atom; empty means no date element.
	read    bool
}

// renderFeed renders the given entries in the requested
// format. format is one of "rss2", "rss1", and "atom".
func renderFeed(format string, entries []feedEntry) string {
	var b strings.Builder
	switch format {
	case "rss2":
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<rss version="2.0"><channel>`)
		b.WriteString(`<title>Test feed</title><link>https://example.com/</link><description>Test description</description>`)
		for _, e := range entries {
			b.WriteString(`<item>`)
			b.WriteString(`<guid>` + e.id + `</guid>`)
			b.WriteString(`<title>` + e.title + `</title>`)
			b.WriteString(`<description>` + e.summary + `</description>`)
			if e.content != "" {
				b.WriteString(`<content:encoded>` + e.content + `</content:encoded>`)
			}
			if e.link != "" {
				b.WriteString(`<link>` + e.link + `</link>`)
			}
			if e.date != "" {
				b.WriteString(`<pubDate>` + e.date + `</pubDate>`)
			}
			b.WriteString(`</item>`)
		}
		b.WriteString(`</channel></rss>`)

	case "rss1":
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">`)
		b.WriteString(`<channel rdf:about="https://example.com/feed.rdf">`)
		b.WriteString(`<title>Test feed</title><link>https://example.com/</link><description>Test description</description>`)
		b.WriteString(`</channel>`)
		for _, e := range entries {
			// The RSS1 parser derives item IDs from the link.
			b.WriteString(`<item rdf:about="` + e.link + `">`)
			b.WriteString(`<title>` + e.title + `</title>`)
			b.WriteString(`<description>` + e.summary + `</description>`)
			if e.content != "" {
				b.WriteString(`<content:encoded>` + e.content + `</content:encoded>`)
			}
			b.WriteString(`<link>` + e.link + `</link>`)
			if e.date != "" {
				b.WriteString(`<pubDate>` + e.date + `</pubDate>`)
			}
			b.WriteString(`</item>`)
		}
		b.WriteString(`</rdf:RDF>`)

	case "atom":
		b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
		b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom">`)
		b.WriteString(`<title>Test feed</title><subtitle>Test description</subtitle>`)
		b.WriteString(`<link href="https://example.com/"/><id>https://example.com/feed</id>`)
		for _, e := range entries {
			b.WriteString(`<entry>`)
			b.WriteString(`<id>` + e.id + `</id>`)
			b.WriteString(`<title>` + e.title + `</title>`)
			b.WriteString(`<summary>` + e.summary + `</summary>`)
			if e.content != "" {
				b.WriteString(`<content type="html">` + e.content + `</content>`)
			}
			if e.link != "" {
				b.WriteString(`<link href="` + e.link + `"/>`)
			}
			if e.date != "" {
				b.WriteString(`<updated>` + e.date + `</updated>`)
			}
			b.WriteString(`</entry>`)
		}
		b.WriteString(`</feed>`)

	default:
		panic(fmt.Sprintf("unknown format %q", format))
	}

	return b.String()
}

// expectedItem builds the Item expected from a parsed
// feedEntry. Dates use the normalised UTC representation.
func expectedItem(t *testing.T, r *Reader, format string, e feedEntry) *Item {
	t.Helper()
	item := &Item{
		Title:   e.title,
		Summary: e.summary,
		Content: e.content,
		ID:      e.id,
		Read:    e.read,
	}

	if format == "rss1" {
		item.ID = e.link
	}

	if e.link != "" {
		item.Links = []*Link{{Href: e.link}}
	}

	if e.date != "" {
		item.Date = r.parseTime(e.date)
		if item.Date.IsZero() {
			t.Fatalf("test setup: failed to parse date %q", e.date)
		}
	}

	return item
}

// checkFeed compares every field of the feed's items
// against the expected entries, along with item count
// and Unread, and returns the items for further checks.
func checkFeed(t *testing.T, r *Reader, format string, feed *Feed, expected []feedEntry, wantUnread uint32) []*Item {
	t.Helper()

	if len(feed.Items) != len(expected) {
		t.Fatalf("item count:\ngot:  %d (%v)\nwant: %d (%v)", len(feed.Items), itemIDs(feed.Items), len(expected), entryIDs(expected))
	}

	for i, e := range expected {
		want := expectedItem(t, r, format, e)
		got := feed.Items[i]

		if !reflect.DeepEqual(got, want) {
			t.Fatalf("item %d (%s) mismatch:\ngot:  %+v\nwant: %+v", i, want.ID, got, want)
		}
	}

	if feed.Unread != wantUnread {
		t.Fatalf("Unread count:\ngot:  %d\nwant: %d", feed.Unread, wantUnread)
	}

	return feed.Items
}

func itemIDs(items []*Item) []string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = item.ID
	}
	return ids
}

func entryIDs(entries []feedEntry) []string {
	ids := make([]string, len(entries))
	for i, e := range entries {
		ids[i] = e.id
	}
	return ids
}

// newTestServer serves successive payloads for a single
// feed URL, one per request.
func newTestServer(t *testing.T, payloads ...string) (*httptest.Server, func()) {
	t.Helper()
	request := 0
	mux := http.NewServeMux()
	mux.HandleFunc("GET /feed", func(w http.ResponseWriter, req *http.Request) {
		if request >= len(payloads) {
			t.Errorf("unexpected extra feed request %d", request+1)
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "application/xml")
		io.Copy(w, strings.NewReader(payloads[request]))
		request++
	})

	srv := httptest.NewServer(mux)
	return srv, func() {
		srv.Close()
		if request != len(payloads) {
			t.Errorf("server got %d requests, want %d", request, len(payloads))
		}
	}
}

// parseAndCheck performs a first parse of firstPayload
// and checks the complete resulting state.
func parseAndCheck(t *testing.T, r *Reader, format, url, firstPayload string, expected []feedEntry, wantUnread uint32) *Feed {
	t.Helper()
	feed, err := r.Parse(url, strings.NewReader(firstPayload))
	if err != nil {
		t.Fatalf("first parse failed: %v", err)
	}
	checkFeed(t, r, format, feed, expected, wantUnread)
	return feed
}

// updateAndCheck makes NextUpdate due, performs an
// update, and checks the complete resulting state.
func updateAndCheck(t *testing.T, r *Reader, ctx context.Context, format string, feed *Feed, expected []feedEntry, wantUnread uint32) {
	t.Helper()
	feed.NextUpdate = time.Time{}
	if err := r.Update(ctx, feed); err != nil {
		t.Fatalf("update failed: %v", err)
	}
	checkFeed(t, r, format, feed, expected, wantUnread)
}

// TestMergeRevisionsDisabledByDefault verifies that
// without WithMergeRevisions, the first item seen for
// each ID is always kept during parsing and updating,
// including for repeated IDs inside one feed.
func TestMergeRevisionsDisabledByDefault(t *testing.T) {
	const (
		rssDate1 = "Mon, 06 Sep 2010 00:01:00 +0000"
		rssDate2 = "Tue, 07 Sep 2010 00:01:00 +0000"
	)

	tests := []struct {
		name   string
		format string
		link   func(id string) string
		date   func(s string) string
	}{
		{"RSS2", "rss2", func(id string) string { return "https://example.com/" + id }, func(s string) string { return s }},
		{"RSS1", "rss1", func(id string) string { return "https://example.com/" + id }, func(s string) string { return s }},
		{"Atom", "atom", func(id string) string { return "https://example.com/" + id }, func(s string) string {
			tv, err := time.Parse(time.RFC1123Z, s)
			if err != nil {
				t.Fatalf("test setup: %v", err)
			}
			return tv.UTC().Format(time.RFC3339)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, err := NewReader()
			if err != nil {
				t.Fatal(err)
			}

			first := []feedEntry{
				{id: "a", title: "A v1", summary: "summary a1", content: "content a1", link: test.link("a"), date: test.date(rssDate1)},
				{id: "b", title: "B v1", summary: "summary b1", link: test.link("b"), date: test.date(rssDate1)},
				// Duplicate ID inside a single feed is ignored.
				{id: "a", title: "A v2 newer", summary: "summary a2", link: test.link("a"), date: test.date(rssDate2)},
			}
			wantFirst := []feedEntry{first[0], first[1]}

			// Second fetch: a newer revision of "a", plus new item "c";
			// "b" is dropped from the feed and must be retained.
			second := []feedEntry{
				{id: "a", title: "A v3 update", summary: "summary a3", link: test.link("a"), date: test.date(rssDate2)},
				{id: "c", title: "C v1", summary: "summary c1", link: test.link("c"), date: test.date(rssDate2)},
			}
			wantSecond := []feedEntry{wantFirst[0], wantFirst[1], second[1]}

			srv, verify := newTestServer(t, renderFeed(test.format, second))
			defer verify()
			url := srv.URL + "/feed"

			// First parse: duplicate ID inside one feed counts once.
			feed := parseAndCheck(t, r, test.format, url, renderFeed(test.format, first), wantFirst, 2)
			original := checkFeed(t, r, test.format, feed, wantFirst, 2)
			ptrA, ptrB := original[0], original[1]

			// Continuous update: revision ignored, new item appended,
			// old item retained, pointers and positions preserved.
			updateAndCheck(t, r, context.Background(), test.format, feed, wantSecond, 3)
			if feed.Items[0] != ptrA || feed.Items[1] != ptrB {
				t.Fatalf("existing item pointers changed after update:\nitem 0 same: %v\nitem 1 same: %v", feed.Items[0] == ptrA, feed.Items[1] == ptrB)
			}
		})
	}
}

// TestMergeRevisionsEnabled covers revision merging
// end to end for RSS1, RSS2, and Atom:
//
//   - first parse with a same-ID duplicate in one feed;
//   - continuous updates with newer and older revisions;
//   - reversed-order revisions (newer first);
//   - zero dates never overwriting non-zero dates;
//   - Read state, position, and pointer preservation;
//   - Unread only counting new IDs once.
func TestMergeRevisionsEnabled(t *testing.T) {
	const (
		rssDate1 = "Mon, 06 Sep 2010 00:01:00 +0000"
		rssDate2 = "Tue, 07 Sep 2010 00:01:00 +0000"
		rssDate3 = "Wed, 08 Sep 2010 00:01:00 +0000"
	)

	tests := []struct {
		name   string
		format string
		link   func(id string) string
		date   func(s string) string
	}{
		{"RSS2", "rss2", func(id string) string { return "https://example.com/" + id }, func(s string) string { return s }},
		{"RSS1", "rss1", func(id string) string { return "https://example.com/" + id }, func(s string) string { return s }},
		{"Atom", "atom", func(id string) string { return "https://example.com/" + id }, func(s string) string {
			tv, err := time.Parse(time.RFC1123Z, s)
			if err != nil {
				t.Fatalf("test setup: %v", err)
			}
			return tv.UTC().Format(time.RFC3339)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r, err := NewReader(WithMergeRevisions())
			if err != nil {
				t.Fatal(err)
			}

			link := test.link
			date := test.date

			// First parse: "a" appears twice in one feed, the
			// second time with a later date; it must be merged
			// but counted once.
			first := []feedEntry{
				{id: "a", title: "A v1", summary: "summary a1", content: "content a1", link: link("a"), date: date(rssDate1)},
				{id: "b", title: "B v1", summary: "summary b1", link: link("b"), date: date(rssDate1)},
				{id: "a", title: "A v2", summary: "summary a2", content: "content a2", link: link("a"), date: date(rssDate2)},
			}
			wantFirst := []feedEntry{
				{id: "a", title: "A v2", summary: "summary a2", content: "content a2", link: link("a"), date: date(rssDate2)},
				{id: "b", title: "B v1", summary: "summary b1", link: link("b"), date: date(rssDate1)},
			}

			// Second fetch: "a" at an even later date, a new "c",
			// and "b" missing (must be retained).
			second := []feedEntry{
				{id: "a", title: "A v3", summary: "summary a3", content: "content a3", link: link("a"), date: date(rssDate3)},
				{id: "c", title: "C v1", summary: "summary c1", link: link("c"), date: date(rssDate3)},
			}
			wantSecond := []feedEntry{
				{id: "a", title: "A v3", summary: "summary a3", content: "content a3", link: link("a"), date: date(rssDate3)},
				{id: "b", title: "B v1", summary: "summary b1", link: link("b"), date: date(rssDate1)},
				{id: "c", title: "C v1", summary: "summary c1", link: link("c"), date: date(rssDate3)},
			}

			// Third fetch: an older revision of "a" is ignored,
			// a zero-date revision of "b" must not overwrite its
			// date, and a zero-date new item "d" is still stored.
			third := []feedEntry{
				{id: "a", title: "A old", summary: "summary a-old", link: link("a"), date: date(rssDate1)},
				{id: "b", title: "B zero-date revision", summary: "summary b-zero", link: link("b"), date: ""},
				{id: "d", title: "D v1 no date", summary: "summary d1", link: link("d"), date: ""},
			}
			wantThird := []feedEntry{
				wantSecond[0],
				wantSecond[1],
				wantSecond[2],
				{id: "d", title: "D v1 no date", summary: "summary d1", link: link("d"), date: ""},
			}

			srv, verify := newTestServer(t,
				renderFeed(test.format, second),
				renderFeed(test.format, third),
			)
			defer verify()
			url := srv.URL + "/feed"
			ctx := context.Background()

			// First parse, including the same-ID duplicate.
			feed := parseAndCheck(t, r, test.format, url, renderFeed(test.format, first), wantFirst, 2)
			if feed.Unread != 2 {
				t.Fatalf("Unread after first parse:\ngot:  %d\nwant: 2", feed.Unread)
			}
			ptrA, ptrB := feed.Items[0], feed.Items[1]

			// Mark "a" as read; revisions must preserve Read.
			feed.Items[0].Read = true

			// Continuous update with a newer revision and a new ID.
			// Unread is a caller-managed counter: the merged
			// revision adds nothing, and the one new ID adds one.
			updateAndCheck(t, r, ctx, test.format, feed, markRead(wantSecond, "a"), 3)
			if feed.Unread != 3 {
				t.Fatalf("Unread after second fetch:\ngot:  %d\nwant: 3 (revision adds no Unread, one new item)", feed.Unread)
			}
			if feed.Items[0] != ptrA || feed.Items[1] != ptrB {
				t.Fatalf("existing item pointers changed:\nitem 0 same: %v\nitem 1 same: %v", feed.Items[0] == ptrA, feed.Items[1] == ptrB)
			}
			if !feed.Items[0].Read {
				t.Fatalf("revision of a read item reset its Read state")
			}

			// Third fetch: older revision, zero-date revision, new zero-date item.
			updateAndCheck(t, r, ctx, test.format, feed, markRead(wantThird, "a"), 4)
			if feed.Unread != 4 {
				t.Fatalf("Unread after third fetch:\ngot:  %d\nwant: 4", feed.Unread)
			}
			if feed.Items[0] != ptrA || feed.Items[1] != ptrB {
				t.Fatalf("existing item pointers changed after third fetch:\nitem 0 same: %v\nitem 1 same: %v", feed.Items[0] == ptrA, feed.Items[1] == ptrB)
			}
			if feed.Items[0].Title != "A v3" {
				t.Fatalf("older revision overwrote newer item: got title %q", feed.Items[0].Title)
			}
			if feed.Items[1].Title != "B v1" || feed.Items[1].Date.IsZero() {
				t.Fatalf("zero-date revision overwrote non-zero item: %+v", feed.Items[1])
			}
		})
	}
}

// markRead returns a copy of entries with the entry with
// the given ID marked as read.
func markRead(entries []feedEntry, id string) []feedEntry {
	out := make([]feedEntry, len(entries))
	copy(out, entries)
	for i := range out {
		if out[i].id == id {
			out[i].read = true
		}
	}
	return out
}

// TestMergeRevisionsReversedOrder feeds revisions in
// reverse chronological order across updates and within
// a single feed, verifying the latest date wins and
// equal dates keep the first occurrence.
func TestMergeRevisionsReversedOrder(t *testing.T) {
	const (
		rssDate1 = "Mon, 06 Sep 2010 00:01:00 +0000"
		rssDate2 = "Tue, 07 Sep 2010 00:01:00 +0000"
	)

	formats := []struct {
		name   string
		format string
	}{
		{"RSS2", "rss2"},
		{"RSS1", "rss1"},
		{"Atom", "atom"},
	}

	for _, test := range formats {
		t.Run(test.name, func(t *testing.T) {
			r, err := NewReader(WithMergeRevisions())
			if err != nil {
				t.Fatal(err)
			}

			link := func(id string) string { return "https://example.com/" + id }
			date := func(s string) string {
				if test.format == "atom" {
					tv, _ := time.Parse(time.RFC1123Z, s)
					return tv.UTC().Format(time.RFC3339)
				}
				return s
			}

			// Newer version appears first inside one feed;
			// the older duplicate must not replace it.
			first := []feedEntry{
				{id: "a", title: "A newer first", summary: "newer", link: link("a"), date: date(rssDate2)},
				{id: "a", title: "A older second", summary: "older", link: link("a"), date: date(rssDate1)},
			}
			wantFirst := []feedEntry{first[0]}

			// Equal-date revision: first occurrence is kept.
			second := []feedEntry{
				{id: "a", title: "A equal date challenger", summary: "equal", link: link("a"), date: date(rssDate2)},
			}

			srv, verify := newTestServer(t, renderFeed(test.format, second))
			defer verify()
			url := srv.URL + "/feed"

			feed := parseAndCheck(t, r, test.format, url, renderFeed(test.format, first), wantFirst, 1)
			updateAndCheck(t, r, context.Background(), test.format, feed, wantFirst, 1)
		})
	}
}

// TestMergeRevisionsConvertEntrypoints checks that the
// direct ConvertRSS1, ConvertRSS2, and ConvertAtom1
// entrypoints apply the same revision policy, including
// duplicate IDs within one parsed feed.
func TestMergeRevisionsConvertEntrypoints(t *testing.T) {
	t.Run("RSS2", func(t *testing.T) {
		r, err := NewReader(WithMergeRevisions())
		if err != nil {
			t.Fatal(err)
		}

		payload := renderFeed("rss2", []feedEntry{
			{id: "a", title: "A v1", summary: "s1", link: "https://example.com/a", date: "Mon, 06 Sep 2010 00:01:00 +0000"},
			{id: "a", title: "A v2", summary: "s2", link: "https://example.com/a", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
		})

		parsed, err := rss2.Parse(strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		feed, err := r.ConvertRSS2(defaultURL, parsed)
		if err != nil {
			t.Fatal(err)
		}
		checkFeed(t, r, "rss2", feed, []feedEntry{
			{id: "a", title: "A v2", summary: "s2", link: "https://example.com/a", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
		}, 1)

		// Same input through the default reader keeps v1.
		defaultReader, err := NewReader()
		if err != nil {
			t.Fatal(err)
		}
		parsed, err = rss2.Parse(strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		feed, err = defaultReader.ConvertRSS2(defaultURL, parsed)
		if err != nil {
			t.Fatal(err)
		}
		checkFeed(t, defaultReader, "rss2", feed, []feedEntry{
			{id: "a", title: "A v1", summary: "s1", link: "https://example.com/a", date: "Mon, 06 Sep 2010 00:01:00 +0000"},
		}, 1)
	})

	t.Run("RSS1", func(t *testing.T) {
		r, err := NewReader(WithMergeRevisions())
		if err != nil {
			t.Fatal(err)
		}

		payload := renderFeed("rss1", []feedEntry{
			{id: "a", title: "A v1", summary: "s1", link: "https://example.com/a", date: "Mon, 06 Sep 2010 00:01:00 +0000"},
			{id: "a", title: "A v2", summary: "s2", link: "https://example.com/a", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
		})

		parsed, err := rss1.Parse(strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		feed, err := r.ConvertRSS1(defaultURL, parsed)
		if err != nil {
			t.Fatal(err)
		}
		checkFeed(t, r, "rss1", feed, []feedEntry{
			{id: "a", title: "A v2", summary: "s2", link: "https://example.com/a", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
		}, 1)
	})

	t.Run("Atom", func(t *testing.T) {
		atomDate := func(s string) string {
			tv, _ := time.Parse(time.RFC1123Z, s)
			return tv.UTC().Format(time.RFC3339)
		}

		r, err := NewReader(WithMergeRevisions())
		if err != nil {
			t.Fatal(err)
		}

		payload := renderFeed("atom", []feedEntry{
			{id: "urn:a", title: "A v1", summary: "s1", link: "https://example.com/a", date: atomDate("Mon, 06 Sep 2010 00:01:00 +0000")},
			{id: "urn:a", title: "A v2", summary: "s2", link: "https://example.com/a", date: atomDate("Tue, 07 Sep 2010 00:01:00 +0000")},
		})

		parsed, err := atom1.Parse(strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		feed, err := r.ConvertAtom1(defaultURL, parsed)
		if err != nil {
			t.Fatal(err)
		}
		checkFeed(t, r, "atom", feed, []feedEntry{
			{id: "urn:a", title: "A v2", summary: "s2", link: "https://example.com/a", date: atomDate("Tue, 07 Sep 2010 00:01:00 +0000")},
		}, 1)
	})
}

// TestMergeRevisionsZeroDate verifies both directions of
// the zero-date rule within a single feed and across
// updates:
//
//   - a dated revision replaces an item without a date;
//   - a dateless revision never replaces a dated item;
//   - two dateless versions keep the first occurrence.
func TestMergeRevisionsZeroDate(t *testing.T) {
	const dated = "Tue, 07 Sep 2010 00:01:00 +0000"

	r, err := NewReader(WithMergeRevisions())
	if err != nil {
		t.Fatal(err)
	}

	first := []feedEntry{
		{id: "a", title: "A no date", summary: "s1", link: "https://example.com/a", date: ""},
		// Two dateless versions of one ID: the first is kept.
		{id: "b", title: "B no date v1", summary: "b1", link: "https://example.com/b", date: ""},
		{id: "b", title: "B no date v2", summary: "b2", link: "https://example.com/b", date: ""},
	}
	wantFirst := []feedEntry{first[0], first[1]}

	second := []feedEntry{
		// A dated revision replaces the dateless original.
		{id: "a", title: "A dated revision", summary: "s2", link: "https://example.com/a", date: dated},
		// A dateless revision cannot touch a dated item: build
		// one by first giving "c" a date, then a dateless copy.
		{id: "c", title: "C dated", summary: "c1", link: "https://example.com/c", date: dated},
		{id: "c", title: "C dateless challenger", summary: "c2", link: "https://example.com/c", date: ""},
	}
	wantSecond := []feedEntry{
		{id: "a", title: "A dated revision", summary: "s2", link: "https://example.com/a", date: dated},
		{id: "b", title: "B no date v1", summary: "b1", link: "https://example.com/b", date: ""},
		{id: "c", title: "C dated", summary: "c1", link: "https://example.com/c", date: dated},
	}

	srv, verify := newTestServer(t, renderFeed("rss2", second))
	defer verify()
	url := srv.URL + "/feed"

	feed := parseAndCheck(t, r, "rss2", url, renderFeed("rss2", first), wantFirst, 2)
	ptrA, ptrB := feed.Items[0], feed.Items[1]

	updateAndCheck(t, r, context.Background(), "rss2", feed, wantSecond, 3)

	// Existing items keep their position and pointer; the
	// merged "a" is still the first element.
	if feed.Items[0] != ptrA || feed.Items[1] != ptrB {
		t.Fatalf("existing item pointers changed:\nitem 0 same: %v\nitem 1 same: %v", feed.Items[0] == ptrA, feed.Items[1] == ptrB)
	}
	if feed.Items[0].Date.IsZero() {
		t.Fatalf("dated revision failed to replace a dateless item")
	}
	if feed.Items[1].Title != "B no date v1" {
		t.Fatalf("dateless duplicate replaced the first occurrence: %q", feed.Items[1].Title)
	}
	if feed.Items[2].Title != "C dated" {
		t.Fatalf("dateless revision replaced a dated item: %q", feed.Items[2].Title)
	}
}

// TestMergeRevisionsReadItem ensures a revision merged
// into a read item keeps Read=false behaviour for
// genuinely new items and Read=true for the revised
// item, with Unread unchanged by the revision.
func TestMergeRevisionsReadItem(t *testing.T) {
	r, err := NewReader(WithMergeRevisions())
	if err != nil {
		t.Fatal(err)
	}

	first := []feedEntry{
		{id: "a", title: "A v1", summary: "s1", link: "https://example.com/a", date: "Mon, 06 Sep 2010 00:01:00 +0000"},
	}
	second := []feedEntry{
		{id: "a", title: "A v2", summary: "s2", link: "https://example.com/a", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
		{id: "b", title: "B v1", summary: "b1", link: "https://example.com/b", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
	}

	srv, verify := newTestServer(t, renderFeed("rss2", second))
	defer verify()
	url := srv.URL + "/feed"

	feed := parseAndCheck(t, r, "rss2", url, renderFeed("rss2", first), first, 1)
	if feed.Unread != 1 {
		t.Fatalf("Unread after parse:\ngot:  %d\nwant: 1", feed.Unread)
	}

	feed.Items[0].Read = true
	updateAndCheck(t, r, context.Background(), "rss2", feed, []feedEntry{
		{id: "a", title: "A v2", summary: "s2", link: "https://example.com/a", date: "Tue, 07 Sep 2010 00:01:00 +0000", read: true},
		{id: "b", title: "B v1", summary: "b1", link: "https://example.com/b", date: "Tue, 07 Sep 2010 00:01:00 +0000"},
	}, 2)
	if feed.Unread != 2 {
		t.Fatalf("Unread after revision update:\ngot:  %d\nwant: 2 (revision does not add Unread, new item does)", feed.Unread)
	}
}
