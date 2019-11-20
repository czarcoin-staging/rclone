// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package storage

import (
	"bytes"
	"context"
	"sort"
)

// IteratorFunc implements basic iterator
type IteratorFunc func(ctx context.Context, item *ListItem) bool

// Next returns the next item
func (next IteratorFunc) Next(ctx context.Context, item *ListItem) bool { return next(ctx, item) }

// SelectPrefixed keeps only items that have prefix
// items will be reused and modified
// TODO: remove this
func SelectPrefixed(items Items, prefix []byte) Items {
	result := items[:0]
	for _, item := range items {
		if bytes.HasPrefix(item.Key, prefix) {
			result = append(result, item)
		}
	}
	return result
}

// SortAndCollapse sorts items and combines elements based on Delimiter
// items will be reused and modified
// TODO: remove this
func SortAndCollapse(items Items, prefix []byte) Items {
	sort.Sort(items)
	result := items[:0]

	var currentPrefix []byte
	var prefixed bool
	for _, item := range items {
		if prefixed {
			if bytes.HasPrefix(item.Key, currentPrefix) {
				continue
			}
			prefixed = false
		}

		if p := bytes.IndexByte(item.Key[len(prefix):], Delimiter); p >= 0 {
			currentPrefix = item.Key[:len(prefix)+p+1]
			prefixed = true
			result = append(result, ListItem{
				Key:      currentPrefix,
				IsPrefix: true,
			})
		} else {
			result = append(result, item)
		}
	}

	return result
}

// StaticIterator implements an iterator over list of items
type StaticIterator struct {
	Items Items
	Index int
}

// Next returns the next item from the iterator
func (it *StaticIterator) Next(ctx context.Context, item *ListItem) bool {
	if it.Index >= len(it.Items) {
		return false
	}
	*item = it.Items[it.Index]
	it.Index++
	return true
}
