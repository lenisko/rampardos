// TileStore serves vector tiles to the renderer through the binding's
// resource provider, replacing mbgl's MBTilesFileSource on the hot
// path. The native source costs an actor hop plus a freshly parsed SQL
// statement per tile query on its own worker thread; measured on prod
// it accounted for most of a render's wall time (pixel-independent —
// scale=1 and scale=2 renders cost the same). This store is what the
// Node renderer's better-sqlite3 path was, in-process: one shared
// read-only connection pool with a prepared statement, fronted by a
// byte-bounded LRU of decompressed tile blobs.
//
// One TileStore is shared by every worker across all (style, scale)
// pools — per-runtime provider callbacks are thin delegates — so the
// process holds a handful of SQLite connections total instead of one
// native MBTilesFileSource per worker runtime.
//
// The provider callback runs on native worker/network threads and must
// not call map or runtime APIs (binding contract); everything here is
// SQLite, the LRU, and metrics.
package renderer

import (
	"bytes"
	"container/list"
	"database/sql"
	"encoding/binary"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/klauspost/compress/gzip"
	"github.com/lenisko/rampardos/internal/services"
	maplibre "github.com/maplibre/maplibre-native-ffi/bindings/go"
	_ "github.com/mattn/go-sqlite3"
)

// tileURLPrefix is the custom scheme styleprep templates into prepared
// styles. mbgl treats it as a network URL, which routes it to the
// resource provider instead of a native file source.
const tileURLPrefix = "rampardos://tile/"

// tileExpiry keeps mbgl from re-requesting tiles it already holds; the
// dataset only changes via reload, which reopens the store and swaps
// the prepared style (new pools, fresh tile caches).
const tileExpiry = 365 * 24 * time.Hour

type TileStore struct {
	path string

	// mu guards db/stmt against Reopen (dataset activate/combine swaps
	// the mbtiles symlink); readers hold RLock for the query.
	mu   sync.RWMutex
	db   *sql.DB
	stmt *sql.Stmt

	// cmu guards the LRU. Entries hold decompressed tile bytes; the
	// cache is bounded by total bytes, not entry count.
	cmu       sync.Mutex
	lru       *list.List
	index     map[uint64]*list.Element
	sizeBytes int64
	maxBytes  int64
}

type tileEntry struct {
	key  uint64
	data []byte
}

// gzipReaders recycles inflate state across tile decompressions: a
// gzip.Reader carries the full window buffers, and cold-viewport bursts
// decompress dozens of tiles back to back. Hits never touch gzip — the
// LRU stores decompressed blobs — so this only serves misses.
var gzipReaders = sync.Pool{New: func() any { return new(gzip.Reader) }}

// gunzipTile decompresses a gzip-framed tile blob using a pooled reader
// and an exact-size output buffer taken from the gzip ISIZE trailer
// (the uncompressed length mod 2^32 — always the true length at tile
// sizes), so decompression is one allocation and no growth copies.
func gunzipTile(blob []byte) ([]byte, error) {
	zr := gzipReaders.Get().(*gzip.Reader)
	defer gzipReaders.Put(zr)
	if err := zr.Reset(bytes.NewReader(blob)); err != nil {
		return nil, err
	}
	size := binary.LittleEndian.Uint32(blob[len(blob)-4:])
	out := make([]byte, 0, size)
	buf := bytes.NewBuffer(out)
	if _, err := io.Copy(buf, zr); err != nil {
		return nil, err
	}
	// Close validates nothing further here (CRC is checked at EOF) but
	// keeps the reader in a Reset-able state for the pool.
	if err := zr.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func newTileStore(path string, maxBytes int64) (*TileStore, error) {
	s := &TileStore{
		path:     path,
		lru:      list.New(),
		index:    make(map[uint64]*list.Element),
		maxBytes: maxBytes,
	}
	if err := s.open(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *TileStore) open() error {
	// mode=ro: the store never writes; the admin dataset pipeline owns
	// the file. _query_only hardens that at the connection level.
	db, err := sql.Open("sqlite3", "file:"+s.path+"?mode=ro&_query_only=1")
	if err != nil {
		return fmt.Errorf("renderer: open mbtiles %q: %w", s.path, err)
	}
	// A handful of connections serves every worker: queries are
	// point-lookups on the tiles index and take microseconds warm.
	db.SetMaxOpenConns(4)
	stmt, err := db.Prepare("SELECT tile_data FROM tiles WHERE zoom_level = ? AND tile_column = ? AND tile_row = ?")
	if err != nil {
		_ = db.Close()
		return fmt.Errorf("renderer: prepare tile query on %q: %w", s.path, err)
	}
	s.db = db
	s.stmt = stmt
	return nil
}

// Reopen swaps to the current dataset file (the mbtiles path is a
// symlink the admin pipeline retargets) and drops the tile cache.
func (s *TileStore) Reopen() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stmt != nil {
		_ = s.stmt.Close()
	}
	if s.db != nil {
		_ = s.db.Close()
	}
	if err := s.open(); err != nil {
		return err
	}
	s.cmu.Lock()
	s.lru.Init()
	s.index = make(map[uint64]*list.Element)
	s.sizeBytes = 0
	s.cmu.Unlock()
	s.reportCacheSize()
	return nil
}

func (s *TileStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stmt != nil {
		_ = s.stmt.Close()
		s.stmt = nil
	}
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
}

// Metadata reads the mbtiles metadata table (name → value).
func (s *TileStore) Metadata() (map[string]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("renderer: tile store is closed")
	}
	rows, err := s.db.Query("SELECT name, value FROM metadata")
	if err != nil {
		return nil, fmt.Errorf("renderer: read mbtiles metadata: %w", err)
	}
	defer rows.Close()
	meta := make(map[string]string)
	for rows.Next() {
		var name, value string
		if err := rows.Scan(&name, &value); err != nil {
			return nil, err
		}
		meta[name] = value
	}
	return meta, rows.Err()
}

// TileJSON builds the inline source object styleprep embeds in prepared
// styles: a tiles template on the provider scheme plus the zoom range
// and bounds from the dataset. maxzoom is what makes overzoom work —
// requests above it fetch the parent tile, exactly as the native
// mbtiles source behaved.
func (s *TileStore) TileJSON() (map[string]any, error) {
	meta, err := s.Metadata()
	if err != nil {
		return nil, err
	}
	tj := map[string]any{
		"type":  "vector",
		"tiles": []any{tileURLPrefix + "{z}/{x}/{y}.pbf"},
	}
	if v, err := strconv.Atoi(meta["minzoom"]); err == nil {
		tj["minzoom"] = v
	}
	if v, err := strconv.Atoi(meta["maxzoom"]); err == nil {
		tj["maxzoom"] = v
	} else {
		return nil, fmt.Errorf("renderer: mbtiles metadata has no usable maxzoom (%q)", meta["maxzoom"])
	}
	if b := meta["bounds"]; b != "" {
		parts := strings.Split(b, ",")
		if len(parts) == 4 {
			bounds := make([]any, 0, 4)
			ok := true
			for _, p := range parts {
				f, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
				if err != nil {
					ok = false
					break
				}
				bounds = append(bounds, f)
			}
			if ok {
				tj["bounds"] = bounds
			}
		}
	}
	return tj, nil
}

func tileKey(z, x, y uint32) uint64 {
	return uint64(z)<<58 | uint64(x)<<29 | uint64(y)
}

// Get returns the decompressed tile blob, whether it exists, and any
// error. Missing tiles are a normal outcome for sparse datasets.
func (s *TileStore) Get(z, x, y uint32) ([]byte, bool, error) {
	key := tileKey(z, x, y)

	s.cmu.Lock()
	if el, ok := s.index[key]; ok {
		s.lru.MoveToFront(el)
		data := el.Value.(*tileEntry).data
		s.cmu.Unlock()
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordTileProviderRequest("lru_hit", 0)
		}
		return data, true, nil
	}
	s.cmu.Unlock()

	start := time.Now()
	s.mu.RLock()
	stmt := s.stmt
	if stmt == nil {
		s.mu.RUnlock()
		return nil, false, fmt.Errorf("renderer: tile store is closed")
	}
	// mbtiles rows are TMS: flip the XYZ row.
	tmsY := (uint32(1)<<z - 1) - y
	var blob []byte
	err := stmt.QueryRow(z, x, tmsY).Scan(&blob)
	s.mu.RUnlock()
	if err == sql.ErrNoRows {
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordTileProviderRequest("not_found", time.Since(start).Seconds())
		}
		return nil, false, nil
	}
	if err != nil {
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordTileProviderRequest("error", time.Since(start).Seconds())
		}
		return nil, false, fmt.Errorf("renderer: tile query z=%d x=%d y=%d: %w", z, x, y, err)
	}

	// MVT blobs in mbtiles are conventionally gzip-compressed; mbgl's
	// network path expects decompressed bytes (HTTP would have undone
	// Content-Encoding before mbgl saw them).
	if len(blob) >= 18 && blob[0] == 0x1f && blob[1] == 0x8b {
		blob, err = gunzipTile(blob)
		if err != nil {
			return nil, false, fmt.Errorf("renderer: tile gunzip z=%d x=%d y=%d: %w", z, x, y, err)
		}
	}
	if services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordTileProviderRequest("sqlite_hit", time.Since(start).Seconds())
	}

	s.cmu.Lock()
	if _, ok := s.index[key]; !ok {
		el := s.lru.PushFront(&tileEntry{key: key, data: blob})
		s.index[key] = el
		s.sizeBytes += int64(len(blob))
		for s.sizeBytes > s.maxBytes && s.lru.Len() > 0 {
			victim := s.lru.Back()
			entry := victim.Value.(*tileEntry)
			s.lru.Remove(victim)
			delete(s.index, entry.key)
			s.sizeBytes -= int64(len(entry.data))
			if services.GlobalMetrics != nil {
				services.GlobalMetrics.IncTileProviderEvictions()
			}
		}
	}
	s.cmu.Unlock()
	s.reportCacheSize()
	return blob, true, nil
}

func (s *TileStore) reportCacheSize() {
	if services.GlobalMetrics == nil {
		return
	}
	s.cmu.Lock()
	n := s.sizeBytes
	s.cmu.Unlock()
	services.GlobalMetrics.SetTileProviderCacheBytes(n)
}

// parseTileURL extracts z/x/y from "rampardos://tile/{z}/{x}/{y}.pbf"
// after mbgl's template expansion.
func parseTileURL(u string) (z, x, y uint32, ok bool) {
	rest, found := strings.CutPrefix(u, tileURLPrefix)
	if !found {
		return 0, 0, 0, false
	}
	rest = strings.TrimSuffix(rest, ".pbf")
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	vals := make([]uint32, 3)
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return 0, 0, 0, false
		}
		vals[i] = uint32(v)
	}
	if vals[0] > 30 {
		return 0, 0, 0, false
	}
	return vals[0], vals[1], vals[2], true
}

// Provide is the binding resource-provider callback, shared by every
// worker runtime. Only provider-scheme tile URLs are handled; anything
// else (file:// glyphs and sprites never get here, external HTTP does)
// passes through to native networking.
func (s *TileStore) Provide(req maplibre.ResourceRequest, h *maplibre.ResourceRequestHandle) maplibre.ResourceProviderDecision {
	u := req.RequestedURL
	if !strings.HasPrefix(u, tileURLPrefix) {
		u = req.ResolvedURL
	}
	z, x, y, ok := parseTileURL(u)
	if !ok {
		if services.GlobalMetrics != nil {
			services.GlobalMetrics.RecordTileProviderRequest("passthrough", 0)
		}
		return maplibre.ResourceProviderDecisionPassThrough
	}

	data, found, err := s.Get(z, x, y)
	response := maplibre.ResourceResponse{}
	switch {
	case err != nil:
		response.Status = maplibre.ResourceResponseStatusError
		response.ErrorReason = maplibre.ResourceErrorReasonOther
		response.ErrorMessage = err.Error()
	case !found:
		response.Status = maplibre.ResourceResponseStatusNoContent
	default:
		response.Status = maplibre.ResourceResponseStatusOK
		response.Bytes = data
		response.HasExpires = true
		response.ExpiresUnixMS = time.Now().Add(tileExpiry).UnixMilli()
	}
	if cerr := h.Complete(response); cerr != nil && services.GlobalMetrics != nil {
		services.GlobalMetrics.RecordTileProviderRequest("error", 0)
	}
	return maplibre.ResourceProviderDecisionHandle
}
