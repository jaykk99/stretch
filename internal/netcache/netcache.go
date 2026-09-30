// Package netcache is a deduplicing fetch cache: download a URL once,
// serve it N times locally.
//
// Bodies are content-addressed by SHA-256 and zstd-compressed at rest, so
// two different URLs with identical bytes share one stored copy. A manifest
// maps URL -> body hash. Bytes-saved accounting tracks network transfers
// avoided.
//
// Honest limits, stated up front:
//   - v1 caches by URL with NO revalidation (no ETag/If-Modified-Since).
//     A cached URL is served as-is until --refresh. Stale data is the
//     documented tradeoff, not a bug.
//   - This saves bandwidth on repeated fetches. It does not make the first
//     fetch faster, and it cannot cache what changes every request.
package netcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// MaxBody caps a single fetched body (2GB). Larger bodies are refused
// rather than half-cached.
const MaxBody = 2 << 30

type urlEntry struct {
	Hash    string    `json:"hash"`
	Fetched time.Time `json:"fetched"`
	Size    int64     `json:"size"`
}

// Stats is the honest bandwidth accounting.
type Stats struct {
	URLs           int    `json:"urls"`
	NetworkFetches uint64 `json:"network_fetches"`
	CacheHits      uint64 `json:"cache_hits"`
	BytesFetched   uint64 `json:"bytes_fetched"` // from the network
	BytesServed    uint64 `json:"bytes_served"`  // from cache
	StoredBodies   int    `json:"stored_bodies"` // unique bodies on disk
	StoredBytes    uint64 `json:"stored_bytes"`  // compressed bytes on disk
}

// SavedBytes = bytes served from cache that otherwise would be re-downloaded.
func (s Stats) SavedBytes() uint64 { return s.BytesServed }

type Cache struct {
	mu       sync.Mutex
	dir      string
	bodies   string
	manifest map[string]urlEntry
	client   *http.Client
	enc      *zstd.Encoder
	dec      *zstd.Decoder
	stats    Stats
}

// New opens (or creates) the cache directory. client may be nil (default).
func New(dir string, client *http.Client) (*Cache, error) {
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}
	bodies := filepath.Join(dir, "bodies")
	if err := os.MkdirAll(bodies, 0o755); err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, bodies: bodies, manifest: make(map[string]urlEntry), client: client, enc: enc, dec: dec}
	if data, err := os.ReadFile(filepath.Join(dir, "manifest.json")); err == nil {
		json.Unmarshal(data, &c.manifest)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "stats.json")); err == nil {
		json.Unmarshal(data, &c.stats)
	}
	return c, nil
}

func (c *Cache) save() {
	md, _ := json.Marshal(c.manifest)
	os.WriteFile(filepath.Join(c.dir, "manifest.json"), md, 0o644)
	sd, _ := json.Marshal(c.stats)
	os.WriteFile(filepath.Join(c.dir, "stats.json"), sd, 0o644)
}

func (c *Cache) bodyPath(hash string) string { return filepath.Join(c.bodies, hash+".zst") }

// readBody loads and decompresses a stored body; false if missing/corrupt.
func (c *Cache) readBody(hash string) ([]byte, bool) {
	comp, err := os.ReadFile(c.bodyPath(hash))
	if err != nil {
		return nil, false
	}
	raw, err := c.dec.DecodeAll(comp, nil)
	if err != nil {
		return nil, false
	}
	h := sha256.Sum256(raw)
	if hex.EncodeToString(h[:]) != hash {
		return nil, false // corrupt: hash mismatch, refetch below
	}
	return raw, true
}

// Fetch returns the URL's bytes. fromCache reports whether the network was
// touched. With refresh=true the network is always used.
func (c *Cache) Fetch(url string, refresh bool) (data []byte, fromCache bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !refresh {
		if e, ok := c.manifest[url]; ok {
			if body, ok := c.readBody(e.Hash); ok {
				c.stats.CacheHits++
				c.stats.BytesServed += uint64(len(body))
				c.save()
				return body, true, nil
			}
			// Entry exists but body missing/corrupt: fall through to refetch.
		}
	}
	resp, err := c.client.Get(url)
	if err != nil {
		return nil, false, fmt.Errorf("fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("fetch %s: HTTP %s", url, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBody+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > MaxBody {
		return nil, false, fmt.Errorf("body exceeds %d bytes, refused", MaxBody)
	}
	h := sha256.Sum256(body)
	hash := hex.EncodeToString(h[:])
	bp := c.bodyPath(hash)
	if _, err := os.Stat(bp); err != nil {
		tmp := bp + ".tmp"
		if werr := os.WriteFile(tmp, c.enc.EncodeAll(body, nil), 0o644); werr != nil {
			return nil, false, werr
		}
		if werr := os.Rename(tmp, bp); werr != nil {
			return nil, false, werr
		}
	}
	c.manifest[url] = urlEntry{Hash: hash, Fetched: time.Now(), Size: int64(len(body))}
	c.stats.URLs = len(c.manifest)
	c.stats.NetworkFetches++
	c.stats.BytesFetched += uint64(len(body))
	c.save()
	return body, false, nil
}

// Stats returns bandwidth accounting plus current disk usage.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.stats
	ents, _ := os.ReadDir(c.bodies)
	st.StoredBodies = 0
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			st.StoredBodies++
			st.StoredBytes += uint64(info.Size())
		}
	}
	return st
}

// Clear removes cached bodies and the manifest (stats kept).
func (c *Cache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := os.RemoveAll(c.bodies); err != nil {
		return err
	}
	if err := os.MkdirAll(c.bodies, 0o755); err != nil {
		return err
	}
	c.manifest = make(map[string]urlEntry)
	c.save()
	return nil
}
