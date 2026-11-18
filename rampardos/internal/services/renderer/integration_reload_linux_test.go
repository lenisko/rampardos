//go:build renderer_integration

package renderer

import (
	"context"
	"database/sql"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestReloadStylesFollowsDatasetSwitch guards the reload contract after a
// dataset activate: the mbtiles symlink is retargeted, ReloadStyles runs,
// and the next render must show the new dataset. Reloading the style in
// place on existing maps did not: a map keeps the tiles it has loaded, so
// it went on rendering the old dataset, with the tile provider on or off.
func TestReloadStylesFollowsDatasetSwitch(t *testing.T) {
	for _, provider := range []string{"on", "off"} {
		t.Run("provider="+provider, func(t *testing.T) {
			if provider == "off" {
				t.Setenv("RENDERER_TILE_PROVIDER", "off")
			}
			dir := t.TempDir()
			// Dataset A has a "water" polygon over the whole world tile;
			// dataset B has the same polygon on a layer the style ignores.
			dsA := filepath.Join(dir, "a.mbtiles")
			dsB := filepath.Join(dir, "b.mbtiles")
			writeSingleTileMbtiles(t, dsA, "water")
			writeSingleTileMbtiles(t, dsB, "other")
			link := filepath.Join(dir, "Combined.mbtiles")
			if err := os.Symlink(dsA, link); err != nil {
				t.Fatal(err)
			}

			styleDir := filepath.Join(dir, "ds")
			if err := os.MkdirAll(styleDir, 0o755); err != nil {
				t.Fatal(err)
			}
			style := `{"version":8,"sources":{"v":{"type":"vector","url":"mbtiles://x"}},"layers":[
				{"id":"bg","type":"background","paint":{"background-color":"#ffffff"}},
				{"id":"water","type":"fill","source":"v","source-layer":"water","paint":{"fill-color":"#0000ff"}}]}`
			if err := os.WriteFile(filepath.Join(styleDir, "style.json"), []byte(style), 0o644); err != nil {
				t.Fatal(err)
			}

			r, err := NewGoPoolRenderer(Config{
				PoolSize:       1,
				StylePoolSize:  1,
				RenderTimeout:  15 * time.Second,
				StartupTimeout: 30 * time.Second,
				StylesDir:      dir,
				FontsDir:       dir,
				MbtilesFile:    link,
				DiscoverStyles: func() ([]string, error) { return []string{"ds"}, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()

			centre := func(what string) color.NRGBA {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				img, err := r.RenderViewportImage(ctx, ViewportRequest{
					StyleID: "ds", Latitude: 0, Longitude: 0, Zoom: 1,
					Width: 64, Height: 64, Scale: 1,
				})
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				return img.NRGBAAt(32, 32)
			}
			blue := color.NRGBA{0, 0, 255, 255}
			white := color.NRGBA{255, 255, 255, 255}

			if got := centre("dataset A"); got != blue {
				t.Fatalf("dataset A: centre pixel %v, want water %v", got, blue)
			}

			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(dsB, link); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := r.ReloadStyles(ctx); err != nil {
				t.Fatalf("reload: %v", err)
			}

			if got := centre("dataset B"); got != white {
				t.Fatalf("dataset B after reload: centre pixel %v, want background %v (old dataset still rendered)", got, white)
			}
		})
	}
}

// writeSingleTileMbtiles writes an mbtiles whose only tile, z0/0/0, holds
// one polygon on the named layer covering the whole tile.
func writeSingleTileMbtiles(t *testing.T, path, layer string) {
	t.Helper()
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE tiles (zoom_level INTEGER, tile_column INTEGER, tile_row INTEGER, tile_data BLOB)`,
		`CREATE TABLE metadata (name TEXT, value TEXT)`,
		`INSERT INTO metadata VALUES ('name','test'),('format','pbf'),('minzoom','0'),('maxzoom','0')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if _, err := db.Exec(`INSERT INTO tiles VALUES (0, 0, 0, ?)`, squareTile(layer)); err != nil {
		t.Fatal(err)
	}
}

// squareTile encodes a Mapbox Vector Tile with one layer holding one
// polygon a little larger than the 4096 extent.
func squareTile(layer string) []byte {
	varint := func(b []byte, v uint64) []byte {
		for v >= 0x80 {
			b = append(b, byte(v)|0x80)
			v >>= 7
		}
		return append(b, byte(v))
	}
	field := func(b []byte, num int, payload []byte) []byte {
		b = varint(b, uint64(num<<3|2))
		b = varint(b, uint64(len(payload)))
		return append(b, payload...)
	}
	zz := func(n int64) uint64 { return uint64((n << 1) ^ (n >> 63)) }

	// MoveTo(-64,-64), LineTo +4224,0 / 0,+4224 / -4224,0, ClosePath:
	// clockwise in tile space, so an exterior ring.
	var geom []byte
	for _, v := range []uint64{1 | 1<<3, zz(-64), zz(-64), 2 | 3<<3, zz(4224), zz(0), zz(0), zz(4224), zz(-4224), zz(0), 7 | 1<<3} {
		geom = varint(geom, v)
	}
	var feature []byte
	feature = varint(feature, 3<<3) // type
	feature = varint(feature, 3)    // POLYGON
	feature = field(feature, 4, geom)

	var l []byte
	l = varint(l, 15<<3) // version
	l = varint(l, 2)
	l = field(l, 1, []byte(layer))
	l = field(l, 2, feature)
	l = varint(l, 5<<3) // extent
	l = varint(l, 4096)

	return field(nil, 3, l)
}
