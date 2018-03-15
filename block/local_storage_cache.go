package block

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/rs/zerolog/log"

	"github.com/kopia/kopia/storage"
)

const (
	sweepCacheFrequency = 1 * time.Minute
	fullListCacheItem   = "list-full"
	activeListCacheItem = "list-active"
)

type diskBlockCache struct {
	st                storage.Storage
	cacheStorage      storage.Storage
	maxSizeBytes      int64
	listCacheDuration time.Duration
	hmacSecret        []byte

	mu                 sync.Mutex
	lastTotalSizeBytes int64

	closed chan struct{}
}

func (c *diskBlockCache) getBlock(virtualBlockID, physicalBlockID string, offset, length int64) ([]byte, error) {
	b, err := c.cacheStorage.GetBlock(virtualBlockID, 0, -1)
	if err == nil {
		b, err := c.verifyHMAC(b)
		if err == nil {
			// retrieved from cache and HMAC valid
			return b, nil
		}

		// ignore malformed blocks
		log.Warn().Msgf("malformed block %v: %v", virtualBlockID, err)
	} else if err != storage.ErrBlockNotFound {
		log.Warn().Msgf("unable to read cache %v: %v", virtualBlockID, err)
	}

	b, err = c.st.GetBlock(physicalBlockID, offset, length)
	if err == storage.ErrBlockNotFound {
		// not found in underlying storage
		return nil, err
	}

	if err == nil {
		if c.cacheStorage.PutBlock(virtualBlockID, c.appendHMAC(b)); err != nil {
			log.Warn().Msgf("unable to write cache items %v: %v", virtualBlockID, err)
		}
	}

	return b, err
}

func applyOffsetAndLength(b []byte, offset, length int64) ([]byte, error) {
	if offset > int64(len(b)) {
		return nil, fmt.Errorf("offset of bounds (offset=%v, length=%v, actual length=%v)", offset, length, len(b))
	}

	if length < 0 {
		return b[offset:], nil
	}

	if offset+length > int64(len(b)) {
		return nil, fmt.Errorf("length of bounds (offset=%v, length=%v, actual length=%v)", offset, length, len(b))
	}

	return b[offset : offset+length], nil
}

func (c *diskBlockCache) putBlock(blockID string, data []byte) error {
	if err := c.st.PutBlock(blockID, data); err != nil {
		return err
	}

	// now write to cache on a best-effort basis
	if err := c.cacheStorage.PutBlock(blockID, c.appendHMAC(data)); err != nil {
		log.Warn().Msgf("unable to write cache item: %v", err)
	}
	c.deleteListCache()
	return nil
}

func (c *diskBlockCache) listIndexBlocks(full bool) ([]Info, error) {
	var cachedListBlockID string

	if full {
		cachedListBlockID = fullListCacheItem
	} else {
		cachedListBlockID = activeListCacheItem
	}

	ci, err := c.readBlocksFromCacheBlock(cachedListBlockID)
	if err == nil {
		expirationTime := ci.Timestamp.Add(c.listCacheDuration)
		if time.Now().Before(expirationTime) {
			log.Debug().Bool("full", full).Str("file", cachedListBlockID).Msg("retrieved index blocks from cache")
			return ci.Blocks, nil
		}
	} else if err != storage.ErrBlockNotFound {
		log.Warn().Msgf("unable to open cache file %v: %v", cachedListBlockID, err)
	}

	log.Debug().Bool("full", full).Msg("listing index blocks from source")
	blocks, err := listIndexBlocksFromStorage(c.st, full)
	if err == nil {
		ci := cachedList{
			Blocks:    blocks,
			Timestamp: time.Now(),
		}
		log.Debug().Bool("full", full).Msgf("saving %v index blocks to cache: %v", len(blocks), cachedListBlockID)
		// save to cache
		if data, err := json.Marshal(ci); err == nil {
			if err := c.cacheStorage.PutBlock(cachedListBlockID, c.appendHMAC(data)); err != nil {
				log.Printf("warning: can't save list: %v", err)
			}
		}
	}

	return blocks, err
}

func (c *diskBlockCache) readBlocksFromCacheBlock(blockID string) (*cachedList, error) {
	ci := &cachedList{}
	data, err := c.cacheStorage.GetBlock(blockID, 0, -1)
	if err != nil {
		return nil, err
	}

	data, err = c.verifyHMAC(data)
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, &ci); err != nil {
		return nil, fmt.Errorf("can't unmarshal cached list results: %v", err)
	}

	return ci, nil

}

func (c *diskBlockCache) readBlocksFromSource(maxCompactions int) ([]Info, error) {
	log.Printf("readBlocksFromSource (maxCompactions=%v)", maxCompactions)
	var blocks []Info
	ch, cancel := c.st.ListBlocks(indexBlockPrefix)
	defer cancel()

	numCompactions := 0
	for e := range ch {
		log.Printf("found block %+v", e)
		if e.Error != nil {
			return nil, e.Error
		}

		blocks = append(blocks, Info{
			BlockID:   e.BlockID,
			Length:    e.Length,
			Timestamp: e.TimeStamp,
		})

		if _, ok := getCompactedTimestamp(e.BlockID); ok {
			numCompactions++
			log.Printf("found compaction %v / %v", numCompactions, maxCompactions)
			if numCompactions >= maxCompactions {
				break
			}
		}
	}
	return blocks, nil
}

func (c *diskBlockCache) appendHMAC(data []byte) []byte {
	h := hmac.New(sha256.New, c.hmacSecret)
	h.Write(data)
	validSignature := h.Sum(nil)
	return append(append([]byte(nil), data...), validSignature...)
}

func (c *diskBlockCache) verifyHMAC(b []byte) ([]byte, error) {
	if len(b) < sha256.Size {
		return nil, errors.New("invalid data - too short")
	}

	p := len(b) - sha256.Size
	data := b[0:p]
	signature := b[p:]
	h := hmac.New(sha256.New, c.hmacSecret)
	h.Write(data)
	validSignature := h.Sum(nil)
	if len(signature) != len(validSignature) {
		return nil, errors.New("invalid signature length")
	}
	if hmac.Equal(validSignature, signature) {
		return data, nil
	}

	return nil, errors.New("invalid data - corrupted")
}

func (c *diskBlockCache) close() error {
	close(c.closed)
	return nil
}

func (c *diskBlockCache) sweepDirectoryPeriodically() {
	for {
		select {
		case <-c.closed:
			return

		case <-time.After(sweepCacheFrequency):
			err := c.sweepDirectory()
			if err != nil {
				log.Printf("warning: blockCache sweep failed: %v", err)
			}
		}
	}
}

func (c *diskBlockCache) sweepDirectory() (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.maxSizeBytes == 0 {
		return nil
	}

	t0 := time.Now()
	log.Debug().Msg("sweeping cache")

	ch, cancel := c.cacheStorage.ListBlocks("")
	defer cancel()

	var items []storage.BlockMetadata

	for it := range ch {
		if it.Error != nil {
			return fmt.Errorf("error listing cache: %v", it.Error)
		}
		items = append(items, it)
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].TimeStamp.After(items[j].TimeStamp)
	})

	var totalRetainedSize int64
	for _, it := range items {
		if totalRetainedSize > c.maxSizeBytes {
			if err := c.cacheStorage.DeleteBlock(it.BlockID); err != nil {
				log.Warn().Msgf("unable to remove %v: %v", it.BlockID, err)
			}
		} else {
			totalRetainedSize += it.Length
		}
	}
	log.Debug().Msgf("finished sweeping directory in %v and retained %v/%v bytes (%v %%)", time.Since(t0), totalRetainedSize, c.maxSizeBytes, 100*totalRetainedSize/c.maxSizeBytes)
	c.lastTotalSizeBytes = totalRetainedSize
	return nil
}

func (c *diskBlockCache) deleteListCache() {
	c.cacheStorage.DeleteBlock(fullListCacheItem)
	c.cacheStorage.DeleteBlock(activeListCacheItem)
}