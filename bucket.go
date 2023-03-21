package directcache

import (
	"bytes"
	"errors"

	"github.com/cespare/xxhash/v2"
)

// bucket indexes and holds entries.
type bucket struct {
	m           vmap                   // maps key hash to offset
	q           fifo                   // the queue buffer stores entries
	shouldEvict func(entry Entry) bool // the custom evention policy
}

// Reset resets the bucket with new capacity and new eviction method.
// If shouldEvict is nil, the default LRU policy is used.
// It drops all entries.
func (b *bucket) Reset(capacity int) {
	b.m.Reset(capacity - 1)
	b.q.Reset(capacity)
}

// SetEvictionPolicy customizes the cache eviction policy.
func (b *bucket) SetEvictionPolicy(shouldEvict func(entry Entry) bool) {
	b.shouldEvict = shouldEvict
}

// Set set val for key.
// false returned and nothing changed if the new entry size exceeds the capacity of this bucket.
func (b *bucket) Set(key []byte, keyHash uint64, valLen int, fn func(val []byte)) bool {
	if offset, found := b.m.Get(keyHash); found {
		ent := b.entryAt(offset)
		if spare := ent.BodySize() - len(key) - valLen; spare >= 0 { // in-place update
			fn(ent.Init(key, valLen, spare))
			ent.AddFlag(recentlyUsedFlag) // avoid evicted too early
			return true
		}
		// key not matched or in-place update failed
		ent.AddFlag(deletedFlag)
	}
	// insert new entry
	if offset, ok := b.insertEntry(key, valLen, 0, fn); ok {
		b.m.Set(keyHash, offset)
		return true
	}
	return false
}

// Del deletes the key.
// false is returned if key does not exist.
func (b *bucket) Del(key []byte, keyHash uint64) bool {
	if offset, found := b.m.Get(keyHash); found {
		if ent := b.entryAt(offset); bytes.Equal(ent.Key(), key) {
			b.m.Del(keyHash)
			ent.AddFlag(deletedFlag)
			return true
		}
	}
	return false
}

// Get get the value for key.
// false is returned if the key not found.
// If peek is true, the entry will not be marked as recently-used.
func (b *bucket) Get(key []byte, keyHash uint64, fn func(val []byte), peek bool) bool {
	if offset, found := b.m.Get(keyHash); found {
		if ent := b.entryAt(offset); bytes.Equal(ent.Key(), key) {
			if !peek {
				ent.AddFlag(recentlyUsedFlag)
			}
			if fn != nil {
				fn(ent.Value())
			}
			return true
		}
	}
	return false
}

// Dump dumps entries.
func (b *bucket) Dump(f func(Entry) bool) bool {
	size := b.q.Size()
	offset := b.q.Front()
	for size > 0 {
		ent := b.entryAt(offset)
		if len(ent) == 0 {
			offset = 0
			continue
		}
		size -= ent.Size()
		offset += ent.Size()
		if !ent.HasFlag(deletedFlag) && !f(ent) {
			return false
		}
	}
	return true
}

// entryAt creates an entry object at the offset of the queue buffer.
func (b *bucket) entryAt(offset int) entry {
	return b.q.Slice(offset)
}

// insertEntry insert a new entry and returns its offset.
// Old entries are evicted like LRU strategy if no enough space.
func (b *bucket) insertEntry(key []byte, valLen int, spare int, fn func(val []byte)) (int, bool) {
	entrySize := entrySize(len(key), valLen, spare)
	if entrySize > b.q.Cap() {
		return 0, false
	}

	pushLimit := 8
	for {
		// have a try
		if offset, ok := b.q.Push(nil, entrySize); ok {
			fn(entry(b.q.Slice(offset)).Init(key, valLen, spare))
			return offset, true
		}

		// no space
		// pop an entry at the front of the queue buffer
		ent := b.entryAt(b.q.Front())
		ent = ent[:ent.Size()]
		if _, ok := b.q.Pop(len(ent)); !ok {
			// will never go here if entry is correctly implemented
			panic(errors.New("bucket.allocEntry: pop entry failed"))
		}

		// good, deleted entry
		if ent.HasFlag(deletedFlag) {
			continue
		}

		keyHash := xxhash.Sum64(ent.Key())
		// pushLimit exceeded
		if pushLimit < 1 {
			b.m.Del(keyHash)
			continue
		}

		if b.shouldEvict == nil {
			// the default LRU policy
			if !ent.HasFlag(recentlyUsedFlag) {
				b.m.Del(keyHash)
				continue
			}
		} else {
			// the custom eviction policy
			if b.shouldEvict(ent) {
				b.m.Del(keyHash)
				continue
			}
		}

		pushLimit--
		ent.RemoveFlag(recentlyUsedFlag)
		//  and push back to the queue
		if offset, ok := b.q.Push(ent, 0); ok {
			// update the offset
			b.m.Set(keyHash, offset)
		} else {
			panic("bucket.allocEntry: push entry failed")
		}
	}
}
