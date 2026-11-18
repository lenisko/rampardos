package renderer

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/gzip"
)

// buildTestMbtiles writes a minimal mbtiles with one gzipped tile at
// XYZ z=2 x=1 y=1 (TMS row 2) and metadata for TileJSON synthesis.
func buildTestMbtiles(t *testing.T) (path string, tileBody []byte) {
	t.Helper()
	return buildTestMbtilesWith(t, []byte("not-really-mvt-but-recognizable-payload"))
}

// buildTestMbtilesWith is buildTestMbtiles with a chosen tile body.
func buildTestMbtilesWith(t *testing.T, tileBody []byte) (path string, _ []byte) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "test.mbtiles")

	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := zw.Write(tileBody); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	stmts := []string{
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`INSERT INTO metadata VALUES ('name','test')`,
		`INSERT INTO metadata VALUES ('format','pbf')`,
		`INSERT INTO metadata VALUES ('minzoom','0')`,
		`INSERT INTO metadata VALUES ('maxzoom','14')`,
		`INSERT INTO metadata VALUES ('bounds','-8.1,49.9,1.8,60.9')`,
	}
	for _, q := range stmts {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// XYZ (2,1,1) → TMS row = (1<<2 - 1) - 1 = 2.
	if _, err := db.Exec(`INSERT INTO tiles VALUES (2, 1, 2, ?)`, gz.Bytes()); err != nil {
		t.Fatalf("insert tile: %v", err)
	}
	return path, tileBody
}

func TestTileStoreGetFlipsTMSAndGunzips(t *testing.T) {
	path, want := buildTestMbtiles(t)
	s, err := newTileStore(path, 1<<20)
	if err != nil {
		t.Fatalf("newTileStore: %v", err)
	}
	defer s.Close()

	data, found, err := s.Get(2, 1, 1)
	if err != nil || !found {
		t.Fatalf("Get(2,1,1) = found=%v err=%v", found, err)
	}
	if !bytes.Equal(data, want) {
		t.Fatalf("Get returned %q, want decompressed %q", data, want)
	}

	// Second read must come from the LRU and return the same bytes.
	again, found, err := s.Get(2, 1, 1)
	if err != nil || !found {
		t.Fatalf("cached Get = found=%v err=%v", found, err)
	}
	if !bytes.Equal(again, want) {
		t.Fatalf("cached Get returned %q, want %q", again, want)
	}

	// Missing tile is found=false, not an error.
	if _, found, err := s.Get(2, 0, 0); err != nil || found {
		t.Fatalf("missing tile: found=%v err=%v, want false/nil", found, err)
	}
}

func TestTileStoreLRUEvictsByBytes(t *testing.T) {
	path, _ := buildTestMbtiles(t)
	// A cache too small for even one tile still serves reads; it just
	// evicts immediately and stays within bounds.
	s, err := newTileStore(path, 8)
	if err != nil {
		t.Fatalf("newTileStore: %v", err)
	}
	defer s.Close()
	if _, found, err := s.Get(2, 1, 1); err != nil || !found {
		t.Fatalf("Get with tiny cache: found=%v err=%v", found, err)
	}
	s.cmu.Lock()
	size, entries := s.sizeBytes, s.lru.Len()
	s.cmu.Unlock()
	if size > 8 || entries != 0 {
		t.Fatalf("cache exceeded bound: %d bytes, %d entries", size, entries)
	}
}

func TestTileStoreTileJSON(t *testing.T) {
	path, _ := buildTestMbtiles(t)
	s, err := newTileStore(path, 1<<20)
	if err != nil {
		t.Fatalf("newTileStore: %v", err)
	}
	defer s.Close()

	tj, err := s.TileJSON()
	if err != nil {
		t.Fatalf("TileJSON: %v", err)
	}
	if tj["maxzoom"] != 14 || tj["minzoom"] != 0 {
		t.Fatalf("TileJSON zooms = %v/%v, want 0/14", tj["minzoom"], tj["maxzoom"])
	}
	tiles, ok := tj["tiles"].([]any)
	if !ok || len(tiles) != 1 || tiles[0] != tileURLPrefix+"{z}/{x}/{y}.pbf" {
		t.Fatalf("TileJSON tiles = %v", tj["tiles"])
	}
	if _, ok := tj["bounds"].([]any); !ok {
		t.Fatalf("TileJSON bounds missing: %v", tj["bounds"])
	}
}

func TestTileStoreReopenFollowsSymlink(t *testing.T) {
	pathA, _ := buildTestMbtiles(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "Combined.mbtiles")
	if err := os.Symlink(pathA, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	s, err := newTileStore(link, 1<<20)
	if err != nil {
		t.Fatalf("newTileStore: %v", err)
	}
	defer s.Close()
	if _, found, _ := s.Get(2, 1, 1); !found {
		t.Fatal("tile missing before reopen")
	}
	if err := s.Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	if _, found, _ := s.Get(2, 1, 1); !found {
		t.Fatal("tile missing after reopen")
	}
}

// A Reopen that fails, say because the symlink briefly points at a file
// that is not there, must leave the store on the previous dataset.
// Closing the old handles before opening the new ones left every later
// uncached read failing with "sql: statement is closed".
func TestTileStoreFailedReopenKeepsPreviousDataset(t *testing.T) {
	pathA, want := buildTestMbtiles(t)
	link := filepath.Join(t.TempDir(), "Combined.mbtiles")
	if err := os.Symlink(pathA, link); err != nil {
		t.Fatal(err)
	}
	s, err := newTileStore(link, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "missing.mbtiles"), link); err != nil {
		t.Fatal(err)
	}
	if err := s.Reopen(); err == nil {
		t.Fatal("Reopen onto a missing file succeeded")
	}

	got, found, err := s.Get(2, 1, 1)
	if err != nil || !found {
		t.Fatalf("Get after a failed Reopen: found=%v err=%v, want the previous dataset's tile", found, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("Get after a failed Reopen = %q, want %q", got, want)
	}
}

// A read that queried the old dataset and finishes after a Reopen must
// not land in the new dataset's cache, or renders would be served the
// old tile until it was evicted.
func TestTileStoreReadFromBeforeReopenIsNotCached(t *testing.T) {
	pathA, oldBody := buildTestMbtiles(t)
	pathB, newBody := buildTestMbtilesWith(t, []byte("the-new-dataset-tile"))
	link := filepath.Join(t.TempDir(), "Combined.mbtiles")
	if err := os.Symlink(pathA, link); err != nil {
		t.Fatal(err)
	}
	s, err := newTileStore(link, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// The first Get has queried dataset A when the dataset switches.
	s.afterQuery = func() {
		s.afterQuery = nil
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(pathB, link); err != nil {
			t.Fatal(err)
		}
		if err := s.Reopen(); err != nil {
			t.Fatal(err)
		}
	}
	got, _, err := s.Get(2, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, oldBody) {
		t.Fatalf("in-flight Get = %q, want the old tile it queried", got)
	}

	got, _, err = s.Get(2, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, newBody) {
		t.Fatalf("Get after Reopen = %q, want the new dataset's %q", got, newBody)
	}
}

func TestParseTileURL(t *testing.T) {
	cases := []struct {
		in      string
		z, x, y uint32
		ok      bool
	}{
		{tileURLPrefix + "14/8190/5447.pbf", 14, 8190, 5447, true},
		{tileURLPrefix + "0/0/0.pbf", 0, 0, 0, true},
		{tileURLPrefix + "14/8190.pbf", 0, 0, 0, false},
		{tileURLPrefix + "a/b/c.pbf", 0, 0, 0, false},
		{tileURLPrefix + "99/0/0.pbf", 0, 0, 0, false},
		{"https://tiles.example/14/8190/5447.pbf", 0, 0, 0, false},
	}
	for _, c := range cases {
		z, x, y, ok := parseTileURL(c.in)
		if ok != c.ok || z != c.z || x != c.x || y != c.y {
			t.Errorf("parseTileURL(%q) = %d/%d/%d ok=%v, want %d/%d/%d ok=%v",
				c.in, z, x, y, ok, c.z, c.x, c.y, c.ok)
		}
	}
}

// A reload racing shutdown may reopen after Close. The store must stay
// closed rather than install fresh handles nothing will close.
func TestTileStoreReopenAfterCloseStaysClosed(t *testing.T) {
	path, _ := buildTestMbtiles(t)
	s, err := newTileStore(path, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := s.Reopen(); err == nil {
		t.Fatal("Reopen after Close succeeded")
	}
	if s.db != nil || s.stmt != nil {
		t.Fatal("Reopen after Close installed new handles")
	}
}
