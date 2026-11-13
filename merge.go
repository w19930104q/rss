package rss

// addItem incorporates item into feed, using index to locate
// existing items by their ID.
//
// Items with a new ID are appended in first-seen order and
// counted as unread exactly once. When the reader is configured
// to merge revisions, an item whose ID is already present may
// replace the existing item in place; otherwise the first item
// seen for each ID is kept and later ones ignored.
//
// Either way, the position and pointer of an existing item are
// preserved, revisions never change Feed.Unread, and the
// existing item's Read state is retained.
//
// addItem reports whether item was added as a new item.
func (r *Reader) addItem(feed *Feed, index map[string]int, item *Item) (added bool) {
	if i, ok := index[item.ID]; ok {
		if r.mergeRevisions {
			mergeRevision(feed.Items[i], item)
		}
		return false
	}

	index[item.ID] = len(feed.Items)
	feed.Items = append(feed.Items, item)
	feed.Unread++
	return true
}

// mergeRevision updates existing in place with the data from
// revision when revision is the newer item. The comparison uses
// the items' normalised Date values:
//
//   - a revision without a date never replaces an item that
//     has one;
//   - a revision with a later date replaces an earlier one,
//     including when the existing item has no date;
//   - items with equal dates keep the one that appeared first.
//
// The existing item's Read state is preserved, and the update
// is done in place so the item's pointer and position are
// retained.
func mergeRevision(existing, revision *Item) {
	if revision.Date.IsZero() || (!existing.Date.IsZero() && !revision.Date.After(existing.Date)) {
		return
	}

	read := existing.Read
	*existing = *revision
	existing.Read = read
}
