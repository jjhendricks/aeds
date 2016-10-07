package kvs

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io/ioutil"
	"time"

	"golang.org/x/net/context"

	"google.golang.org/appengine/datastore"
	"google.golang.org/appengine/memcache"
)

const kind = "kvs"

var NotFound = fmt.Errorf("Key-value pair was not found")

// use App Engine's datastore as a simple key-value store

type KV struct {
	Key     string `datastore:",noindex"`
	Value   []byte `datastore:",noindex"`
	Expires time.Time

	Ttl time.Duration `datastore:"-"` // convenient alternative to Expires
}

// GC defines options for how to perform garbage collection on KV entities.
type GC struct {
	// Ttl describes how much time a single GC operation should be allowed to run.
	// This can be tuned based on how frequently GC jobs are executed.  For
	// example, if GC runs once per minute, you might set Ttl to 50 seconds.
	//
	// Ttl is only a guideline for the GC operation.  It might run longer or
	// shorter than this target.
	//
	// Defaults to 50 seconds.
	Ttl time.Duration

	// Leeway describes how far past its expiration a KV must be before it's
	// considered for garbage collection.  It should be set high enough that
	// the probability of a KV being used again is very low.
	//
	// Defaults to 24 hours.
	Leeway time.Duration
}

// Find looks for an existing key-value pair.  Returns
// NotFound if the key does not exist.
func Find(c context.Context, k string) (*KV, error) {
	// is the kv in memcache?
	kv := new(KV)
	memcacheKey := memKey(k)
	item, err := memcache.Get(c, memcacheKey)
	if err == nil {
		kv.Key = k
		kv.Value = item.Value
		return kv, nil
	}

	// nope, look in the datastore
	key := datastore.NewKey(c, kind, k, 0, nil)
	err = datastore.Get(c, key, kv)
	if err == datastore.ErrNoSuchEntity {
		return nil, NotFound
	}
	if err != nil {
		return nil, err
	}
	if !kv.Expires.IsZero() && kv.Expires.Before(time.Now()) {
		// key has expired. pretend it doesn't exist
		return nil, NotFound
	}

	// store result in memcache for later
	item = &memcache.Item{
		Key:   memcacheKey,
		Value: kv.Value,
	}
	if !kv.Expires.IsZero() {
		item.Expiration = kv.Expires.Sub(time.Now())
	}
	err = memcache.Set(c, item)
	_ = err // memcache is an optimization. ignore its errors.

	return kv, nil
}

func (kv *KV) datastoreKey(c context.Context) *datastore.Key {
	return datastore.NewKey(c, kind, kv.Key, 0, nil)
}

// Put stores a key-value pair until its expiration.
func (kv *KV) Put(c context.Context) error {
	// prepare a memcache item for later
	memcacheKey := memKey(kv.Key)
	item := &memcache.Item{
		Key:   memcacheKey,
		Value: kv.Value,
	}

	// calculate key-value expiration time
	if kv.Ttl > 0 {
		item.Expiration = kv.Ttl
		kv.Expires = time.Now().Add(kv.Ttl)
		kv.Ttl = 0
	} else if !kv.Expires.IsZero() {
		item.Expiration = kv.Expires.Sub(time.Now())
	}

	// store kv into datastore for permanent storage
	_, err := datastore.Put(c, kv.datastoreKey(c), kv)
	if err != nil {
		return err
	}

	// cache kv for faster access next time
	err = memcache.Set(c, item)
	_ = err // memcache is an optimization. ignore errors

	return nil
}

// Remove a rule in the datastore
func (kv *KV) Delete(c context.Context) error {
	// delete from datastore
	err := datastore.Delete(c, kv.datastoreKey(c))
	if err != nil {
		return err
	}

	// delete from memcache too
	err = memcache.Delete(c, memKey(kv.Key))
	_ = err // memcache is an optimization. ignore errors.
	return nil
}

// Compress rewrites the Value field by compressing it with gzip.
func (kv *KV) Compress() error {
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	_, err := w.Write(kv.Value)
	if err != nil {
		return err
	}
	err = w.Close()
	if err != nil {
		return err
	}

	kv.Value = buf.Bytes()
	return nil
}

// Decompress rewrites the Value field by decompressing it with gzip.
func (kv *KV) Decompress() error {
	buf := bytes.NewBuffer(kv.Value)
	r, err := gzip.NewReader(buf)
	if err != nil {
		return err
	}
	val, err := ioutil.ReadAll(r)
	if err != nil {
		return err
	}

	kv.Value = val
	return nil
}

// Encode sets the Value field by gob encoding a Go value.
func (kv *KV) Encode(x interface{}) error {
	var buf bytes.Buffer
	err := gob.NewEncoder(&buf).Encode(x)
	if err != nil {
		return err
	}

	kv.Value = buf.Bytes()
	return nil
}

// Decode extracts the Value field by gob decoding into a Go value.
func (kv *KV) Decode(x interface{}) error {
	buf := bytes.NewBuffer(kv.Value)
	return gob.NewDecoder(buf).Decode(x)
}

// returns a key for use with memcache
func memKey(key string) string {
	return fmt.Sprintf("%s: %s", kind, key)
}

var CollectGarbageTimeout = errors.New("CollectGarbage timed out")

// CollectGarbage deletes expired kv entities from the datastore. This function
// should be called regularly to prevent expired kvs from accumulating in the
// datastore.  Returns the number of entities that were removed from datastore.
//
// If GC.Ttl is reached, returns CollectGarbageTimeout regardless how many
// entities were expired before then.
func CollectGarbage(c context.Context, opts *GC) (int, error) {
	if opts == nil {
		opts = &GC{}
	}
	if opts.Ttl == 0 {
		opts.Ttl = 50 * time.Second
	}
	if opts.Leeway == 0 {
		opts.Leeway = 24 * time.Hour
	}
	quittingTime := time.Now().Add(opts.Ttl)
	cutOff := time.Now().Add(-opts.Leeway)

	n := 0
	q := datastore.NewQuery(kind).
		Filter("Expires<", cutOff).
		Order("Expires").
		Limit(400).
		KeysOnly()
	for {
		if time.Now().After(quittingTime) {
			return n, CollectGarbageTimeout
		}

		keys, err := q.GetAll(c, nil)
		if len(keys) > 0 {
			err = datastore.DeleteMulti(c, keys)
			// don't have to clear memcache. it expires on its own
			if err == nil {
				n += len(keys)
			}
		}
		if err != nil {
			return n, err
		}
		if len(keys) == 0 {
			break
		}
	}

	return n, nil
}
