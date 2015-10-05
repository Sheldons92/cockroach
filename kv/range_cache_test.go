// Copyright 2014 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Matt Tracy (matt.r.tracy@gmail.com)

package kv

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/biogo/store/llrb"
	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/util/leaktest"
	"github.com/cockroachdb/cockroach/util/log"
)

type testDescriptorDB struct {
	data        llrb.Tree
	cache       *rangeDescriptorCache
	lookupCount int
}

type testDescriptorNode struct {
	*roachpb.RangeDescriptor
}

func (a testDescriptorNode) Compare(b llrb.Comparable) int {
	aKey := a.RangeDescriptor.EndKey
	bKey := b.(testDescriptorNode).RangeDescriptor.EndKey
	return bytes.Compare(aKey, bKey)
}

func (db *testDescriptorDB) getDescriptor(key keys.RKey) []roachpb.RangeDescriptor {
	log.Infof("getDescriptor: %s", key)
	response := make([]roachpb.RangeDescriptor, 0, 3)
	for i := 0; i < 3; i++ {
		v := db.data.Ceil(testDescriptorNode{
			&roachpb.RangeDescriptor{
				EndKey: key.Key().Next(),
			},
		})
		if v == nil {
			break
		}
		response = append(response, *(v.(testDescriptorNode).RangeDescriptor))
		// Break to keep from skidding off the end of the available ranges.
		if response[i].EndKey.Equal(roachpb.KeyMax) {
			break
		}
		key = keys.RKey(response[i].EndKey)
	}
	return response
}

func (db *testDescriptorDB) firstRange() (*roachpb.RangeDescriptor, error) {
	return nil, nil
}

func (db *testDescriptorDB) rangeLookup(key keys.RKey, _ lookupOptions, _ *roachpb.RangeDescriptor) ([]roachpb.RangeDescriptor, error) {
	db.lookupCount++
	if bytes.HasPrefix(key, keys.Meta2Prefix) {
		return db.getDescriptor(key[len(keys.Meta2Prefix):]), nil
	}
	return db.getDescriptor(key), nil
}

func (db *testDescriptorDB) splitRange(t *testing.T, key keys.RKey) {
	v := db.data.Ceil(testDescriptorNode{&roachpb.RangeDescriptor{EndKey: key.Key()}})
	if v == nil {
		t.Fatalf("Error splitting range at key %s, range to split not found", string(key))
	}
	val := v.(testDescriptorNode)
	if bytes.Compare(val.EndKey, key) == 0 {
		t.Fatalf("Attempt to split existing range at Endkey: %s", string(key))
	}
	db.data.Insert(testDescriptorNode{
		&roachpb.RangeDescriptor{
			StartKey: val.StartKey,
			EndKey:   key.Key(),
		},
	})
	db.data.Insert(testDescriptorNode{
		&roachpb.RangeDescriptor{
			StartKey: key.Key(),
			EndKey:   val.EndKey,
		},
	})
}

func newTestDescriptorDB() *testDescriptorDB {
	db := &testDescriptorDB{}
	db.data.Insert(testDescriptorNode{
		&roachpb.RangeDescriptor{
			StartKey: keys.MakeKey(keys.Meta2Prefix, roachpb.KeyMin),
			EndKey:   keys.MakeKey(keys.Meta2Prefix, roachpb.KeyMax),
		},
	})
	db.data.Insert(testDescriptorNode{
		&roachpb.RangeDescriptor{
			StartKey: roachpb.KeyMin,
			EndKey:   roachpb.KeyMax,
		},
	})
	return db
}

func (db *testDescriptorDB) assertLookupCount(t *testing.T, expected int, key string) {
	if db.lookupCount != expected {
		t.Errorf("Expected lookup count to be %d after %s, was %d", expected, key, db.lookupCount)
	}
	db.lookupCount = 0
}

func doLookup(t *testing.T, rc *rangeDescriptorCache, key string) *roachpb.RangeDescriptor {
	r, err := rc.LookupRangeDescriptor(keys.RKey(key), lookupOptions{})
	if err != nil {
		t.Fatalf("Unexpected error from LookupRangeDescriptor: %s", err.Error())
	}
	if !r.ContainsKey(keys.KeyAddress(roachpb.Key(key))) {
		t.Fatalf("Returned range did not contain key: %s-%s, %s", r.StartKey, r.EndKey, key)
	}
	log.Infof("doLookup: %s %+v", key, r)
	return r
}

func TestRangeCacheAssumptions(t *testing.T) {
	defer leaktest.AfterTest(t)
	expKeyMin := keys.RangeMetaKey(keys.RangeMetaKey(keys.RangeMetaKey(keys.RKey("test"))))
	if !bytes.Equal(expKeyMin, roachpb.KeyMin) {
		t.Fatalf("RangeCache relies on RangeMetaKey returning KeyMin after two levels, but got %s", expKeyMin)
	}
}

// TestRangeCache is a simple test which verifies that metadata ranges
// are being cached and retrieved properly. It sets up a fake backing
// store for the cache, and measures how often that backing store is
// lookuped when looking up metadata keys through the cache.
func TestRangeCache(t *testing.T) {
	defer leaktest.AfterTest(t)
	db := newTestDescriptorDB()
	for i, char := range "abcdefghijklmnopqrstuvwx" {
		db.splitRange(t, keys.RKey(string(char)))
		if i > 0 && i%6 == 0 {
			db.splitRange(t, keys.RangeMetaKey(keys.RKey(string(char))))
		}
	}

	db.cache = newRangeDescriptorCache(db, 2<<10)

	doLookup(t, db.cache, "aa")
	db.assertLookupCount(t, 2, "aa")

	// Descriptors for the following ranges should be cached.
	doLookup(t, db.cache, "ab")
	db.assertLookupCount(t, 0, "ab")
	doLookup(t, db.cache, "ba")
	db.assertLookupCount(t, 0, "ba")
	doLookup(t, db.cache, "cz")
	db.assertLookupCount(t, 0, "cz")

	// Metadata two ranges weren't cached, same metadata 1 range.
	doLookup(t, db.cache, "d")
	db.assertLookupCount(t, 1, "d")
	doLookup(t, db.cache, "fa")
	db.assertLookupCount(t, 0, "fa")

	// Metadata two ranges weren't cached, metadata 1 was aggressively cached
	doLookup(t, db.cache, "ij")
	db.assertLookupCount(t, 1, "ij")
	doLookup(t, db.cache, "jk")
	db.assertLookupCount(t, 0, "jk")
	doLookup(t, db.cache, "pn")
	db.assertLookupCount(t, 1, "pn")

	// Totally uncached ranges
	doLookup(t, db.cache, "vu")
	db.assertLookupCount(t, 2, "vu")
	doLookup(t, db.cache, "xx")
	db.assertLookupCount(t, 0, "xx")

	// Evict clears one level 1 and one level 2 cache
	db.cache.EvictCachedRangeDescriptor(keys.RKey("da"), nil, false)
	doLookup(t, db.cache, "fa")
	db.assertLookupCount(t, 0, "fa")
	doLookup(t, db.cache, "da")
	db.assertLookupCount(t, 2, "da")

	// Looking up a descriptor that lands on an end-key should work
	// without a cache miss.
	doLookup(t, db.cache, "a")
	db.assertLookupCount(t, 0, "a")

	// Attempt to compare-and-evict with a descriptor that is not equal to the
	// cached one; it should not alter the cache.
	db.cache.EvictCachedRangeDescriptor(keys.RKey("cz"), &roachpb.RangeDescriptor{}, false)
	doLookup(t, db.cache, "cz")
	db.assertLookupCount(t, 0, "cz")
	// Now evict with the actual descriptor. The cache should clear the
	// descriptor and the cached meta key.
	db.cache.EvictCachedRangeDescriptor(keys.RKey("cz"), doLookup(t, db.cache, "cz"), false)
	doLookup(t, db.cache, "cz")
	db.assertLookupCount(t, 2, "cz")

}

// TestRangeCacheClearOverlapping verifies that existing, overlapping
// cached entries are cleared when adding a new entry.
func TestRangeCacheClearOverlapping(t *testing.T) {
	defer leaktest.AfterTest(t)

	defDesc := &roachpb.RangeDescriptor{
		StartKey: roachpb.KeyMin,
		EndKey:   roachpb.KeyMax,
	}

	cache := newRangeDescriptorCache(nil, 2<<10)
	cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey(roachpb.KeyMax))), defDesc)

	// Now, add a new, overlapping set of descriptors.
	minToBDesc := &roachpb.RangeDescriptor{
		StartKey: roachpb.KeyMin,
		EndKey:   roachpb.Key("b"),
	}
	bToMaxDesc := &roachpb.RangeDescriptor{
		StartKey: roachpb.Key("b"),
		EndKey:   roachpb.KeyMax,
	}
	cache.clearOverlappingCachedRangeDescriptors(keys.RKey("b"), keys.RangeMetaKey(keys.RKey("b")), minToBDesc)
	cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey("b"))), minToBDesc)
	if _, desc := cache.getCachedRangeDescriptor(keys.RKey("b"), false); desc != nil {
		t.Errorf("descriptor unexpectedly non-nil: %s", desc)
	}
	cache.clearOverlappingCachedRangeDescriptors(keys.RKey(roachpb.KeyMax), keys.RangeMetaKey(keys.RKey(roachpb.KeyMax)), bToMaxDesc)
	cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey(roachpb.KeyMax))), bToMaxDesc)
	if _, desc := cache.getCachedRangeDescriptor(keys.RKey("b"), false); desc != bToMaxDesc {
		t.Errorf("expected descriptor %s; got %s", bToMaxDesc, desc)
	}

	// Add default descriptor back which should remove two split descriptors.
	cache.clearOverlappingCachedRangeDescriptors(keys.RKey(roachpb.KeyMax), keys.RangeMetaKey(keys.RKey(roachpb.KeyMax)), defDesc)
	cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey(roachpb.KeyMax))), defDesc)
	for _, key := range []keys.RKey{keys.RKey("a"), keys.RKey("b")} {
		if _, desc := cache.getCachedRangeDescriptor(key, false); desc != defDesc {
			t.Errorf("expected descriptor %s for key %s; got %s", defDesc, key, desc)
		}
	}
}

// TestRangeCacheClearOverlappingMeta prevents regression of a bug which caused
// a panic when clearing overlapping descriptors for [KeyMin, Meta2Key). The
// issue was that when attempting to clear out descriptors which were subsumed
// by the above range, an iteration over the corresponding meta keys was
// performed, with the left endpoint excluded. This exclusion was incorrect: it
// first incremented the start key (KeyMin) and then formed the meta key; for
// KeyMin this leads to Meta2Prefix\x00. For the above EndKey, the meta key is
// a Meta1key which sorts before Meta2Prefix\x00, causing a panic. The fix was
// simply to increment the meta key for StartKey, not StartKey itself.
func TestRangeCacheClearOverlappingMeta(t *testing.T) {
	defer leaktest.AfterTest(t)

	firstDesc := &roachpb.RangeDescriptor{
		StartKey: roachpb.KeyMin,
		EndKey:   roachpb.Key("zzz"),
	}
	restDesc := &roachpb.RangeDescriptor{
		StartKey: firstDesc.StartKey,
		EndKey:   roachpb.KeyMax,
	}

	cache := newRangeDescriptorCache(nil, 2<<10)
	cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey(firstDesc.EndKey))),
		firstDesc)
	cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey(restDesc.EndKey))),
		restDesc)

	// Add new range, corresponding to splitting the first range at a meta key.
	metaSplitDesc := &roachpb.RangeDescriptor{
		StartKey: roachpb.KeyMin,
		EndKey:   roachpb.Key(keys.RangeMetaKey(keys.RKey("foo"))),
	}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("invocation of clearOverlappingCachedRangeDescriptors panicked: %v", r)
			}
		}()
		cache.clearOverlappingCachedRangeDescriptors(keys.RKey(metaSplitDesc.EndKey), keys.RangeMetaKey(keys.RKey(metaSplitDesc.EndKey)), metaSplitDesc)
	}()
}

// TestGetCachedRangeDescriptorInclusive verifies the correctness of the result
// that is returned by getCachedRangeDescriptor with inclusive=true.
func TestGetCachedRangeDescriptorInclusive(t *testing.T) {
	defer leaktest.AfterTest(t)

	testData := []*roachpb.RangeDescriptor{
		{StartKey: roachpb.Key("a"), EndKey: roachpb.Key("c")},
		{StartKey: roachpb.Key("c"), EndKey: roachpb.Key("e")},
		{StartKey: roachpb.Key("g"), EndKey: roachpb.Key("z")},
	}

	cache := newRangeDescriptorCache(nil, 2<<10)
	for _, rd := range testData {
		cache.rangeCache.Add(rangeCacheKey(keys.RangeMetaKey(keys.RKey(rd.EndKey))), rd)
	}

	testCases := []struct {
		queryKey keys.RKey
		cacheKey rangeCacheKey
		rng      *roachpb.RangeDescriptor
	}{
		{
			// Check range start key.
			queryKey: keys.RKey("a"),
			cacheKey: nil,
			rng:      nil,
		},
		{
			// Check range end key.
			queryKey: keys.RKey("c"),
			cacheKey: rangeCacheKey(keys.RangeMetaKey(keys.RKey("c"))),
			rng:      &roachpb.RangeDescriptor{StartKey: roachpb.Key("a"), EndKey: roachpb.Key("c")},
		},
		{
			// Check range middle key.
			queryKey: keys.RKey("d"),
			cacheKey: rangeCacheKey(keys.RangeMetaKey(keys.RKey("e"))),
			rng:      &roachpb.RangeDescriptor{StartKey: roachpb.Key("c"), EndKey: roachpb.Key("e")},
		},
		{
			// Check miss range key.
			queryKey: keys.RKey("f"),
			cacheKey: nil,
			rng:      nil,
		},
		{
			// Check range start key with previous range miss.
			queryKey: keys.RKey("g"),
			cacheKey: nil,
			rng:      nil,
		},
	}

	for _, test := range testCases {
		cacheKey, targetRange := cache.getCachedRangeDescriptor(test.queryKey, true /* inclusive */)
		if !reflect.DeepEqual(targetRange, test.rng) {
			t.Fatalf("expect range %v, actual get %v", test.rng, targetRange)
		}
		if !reflect.DeepEqual(cacheKey, test.cacheKey) {
			t.Fatalf("expect cache key %v, actual get %v", test.cacheKey, cacheKey)
		}
	}

}
