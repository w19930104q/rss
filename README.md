rss
=====

 [![GoDoc](https://godoc.org/github.com/SlyMarbo/rss?status.svg)](https://pkg.go.dev/github.com/SlyMarbo/rss/v2)

RSS is a small library for simplifying the parsing of RSS and Atom feeds.
The package conforms to the [RSS 1.0], [RSS 2.0], and [Atom 1.0] specifications.

If you encounter any problems with feeds being parsed incorrectly, please
open an issue on GitHub.

Example usage:

```go
package main

import (
	"context"

	"github.com/SlyMarbo/rss/v2"
)

func main() {
	reader, err := rss.NewReader()
	if err != nil {
		// handle error.
	}

	feed, err := reader.Fetch(context.Background(), "https://example.com/rss")
	if err != nil {
		// handle error.
	}

	// ... Some time later ...

	err = reader.UpdatePatiently(feed)
	if err != nil {
		// handle error.
	}
}
```

The library does its best to follow the appropriate specifications and not to set the `NextUpdate` time
too soon. It currently follows all update time management methods in the RSS 1.0, 2.0, and Atom 1.0
specifications. If one is not provided, it defaults to 12 hour intervals. If you are having issues
with feed providors dropping connections, use `WithDefaultUpdateInterval` to set the default update
interval.

By default, only the first item seen for each ID is retained, both when parsing and when updating.
Feeds occasionally republish an item under the same ID with corrected content and a later date. To
merge such revisions instead of ignoring them, enable `WithMergeRevisions`. When enabled, a later
item with a seen ID replaces the existing item in place when its normalised date is later; items
with equal dates keep the first occurrence, and an item without a date never replaces one that has
one. The existing item's position, pointer, and `Read` state are preserved, revisions never change
`Unread`, and items missing from the newer feed are retained.

The project is not proactively maintained, but I'll respond to issues and PRs as soon as I can.

[Atom 1.0]: https://datatracker.ietf.org/doc/html/rfc4287
[RSS 1.0]: https://web.resource.org/rss/1.0/spec
[RSS 2.0]: https://www.rssboard.org/rss-specification
