package renderer

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

// makeGlyph builds a minimal glyph entry (only the id field, plus a
// distinctive width to verify the body is preserved verbatim through
// the merge). Returns the raw protobuf-encoded body (no outer tag).
func makeGlyph(id, width uint32) []byte {
	var b []byte
	b = protowire.AppendTag(b, fieldGlyphID, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(id))
	b = protowire.AppendTag(b, fieldGlyphWidth, protowire.VarintType)
	b = protowire.AppendVarint(b, uint64(width))
	return b
}

// makePBF builds a single-fontstack glyphs PBF from a list of (id,
// width) pairs. Used as test input.
func makePBF(name, rangeStr string, pairs [][2]uint32) []byte {
	var stack []byte
	stack = protowire.AppendTag(stack, fieldFontstackName, protowire.BytesType)
	stack = protowire.AppendString(stack, name)
	stack = protowire.AppendTag(stack, fieldFontstackRange, protowire.BytesType)
	stack = protowire.AppendString(stack, rangeStr)
	for _, p := range pairs {
		g := makeGlyph(p[0], p[1])
		stack = protowire.AppendTag(stack, fieldFontstackGlyph, protowire.BytesType)
		stack = protowire.AppendBytes(stack, g)
	}
	var out []byte
	out = protowire.AppendTag(out, fieldGlyphsStacks, protowire.BytesType)
	out = protowire.AppendBytes(out, stack)
	return out
}

// extractGlyphSummary parses a composite PBF and returns:
// (name, range, [(id, width)...]). Used for round-trip assertions.
func extractGlyphSummary(t *testing.T, pbf []byte) (string, string, [][2]uint32) {
	t.Helper()
	name, rangeStr, glyphs, err := parseGlyphsPBF(pbf)
	if err != nil {
		t.Fatalf("parseGlyphsPBF: %v", err)
	}
	out := make([][2]uint32, 0, len(glyphs))
	for _, g := range glyphs {
		out = append(out, [2]uint32{g.id, extractGlyphWidth(g.body)})
	}
	return name, rangeStr, out
}

func extractGlyphWidth(glyph []byte) uint32 {
	data := glyph
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		data = data[n:]
		if num == fieldGlyphWidth && typ == protowire.VarintType {
			v, _ := protowire.ConsumeVarint(data)
			return uint32(v)
		}
		data = data[protowire.ConsumeFieldValue(num, typ, data):]
	}
	return 0
}

func TestCombineGlyphs_FirstWins(t *testing.T) {
	// Font A has glyphs 1, 2, 3 with width=10.
	// Font B has glyphs 2, 3, 4, 5 with width=20.
	// Expected merge: A's 1, 2, 3 win; B contributes 4, 5. Widths
	// preserved verbatim from the source.
	a := makePBF("Metropolis Regular", "0-255",
		[][2]uint32{{1, 10}, {2, 10}, {3, 10}})
	b := makePBF("Noto Sans Regular", "0-255",
		[][2]uint32{{2, 20}, {3, 20}, {4, 20}, {5, 20}})

	merged, err := CombineGlyphs("Metropolis Regular,Noto Sans Regular", [][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}

	name, rangeStr, glyphs := extractGlyphSummary(t, merged)

	if want := "Metropolis Regular,Noto Sans Regular"; name != want {
		t.Errorf("name: got %q, want %q", name, want)
	}
	if rangeStr != "0-255" {
		t.Errorf("range: got %q, want %q", rangeStr, "0-255")
	}

	// Order isn't part of the contract, but every id should appear
	// exactly once with the expected width.
	want := map[uint32]uint32{1: 10, 2: 10, 3: 10, 4: 20, 5: 20}
	got := map[uint32]uint32{}
	for _, g := range glyphs {
		if _, dup := got[g[0]]; dup {
			t.Errorf("duplicate id %d in output", g[0])
		}
		got[g[0]] = g[1]
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge result: got %v, want %v", got, want)
	}
}

func TestCombineGlyphs_SingleInputRelabels(t *testing.T) {
	in := makePBF("Noto Sans Regular", "256-511", [][2]uint32{{42, 7}})
	out, err := CombineGlyphs("Metropolis Regular,Noto Sans Regular", [][]byte{in})
	if err != nil {
		t.Fatal(err)
	}
	name, rangeStr, glyphs := extractGlyphSummary(t, out)
	if want := "Metropolis Regular,Noto Sans Regular"; name != want {
		t.Errorf("name: got %q, want %q", name, want)
	}
	if rangeStr != "256-511" {
		t.Errorf("range: got %q, want %q", rangeStr, "256-511")
	}
	if len(glyphs) != 1 || glyphs[0] != [2]uint32{42, 7} {
		t.Errorf("glyphs: got %v, want [{42 7}]", glyphs)
	}
}

func TestCombineGlyphs_NoInputs(t *testing.T) {
	if _, err := CombineGlyphs("X", nil); err == nil {
		t.Error("expected error for empty inputs")
	}
}

func TestCombineGlyphs_RangeTakenFromFirst(t *testing.T) {
	// Pathological: inputs disagree on range. First wins; this
	// matches what every well-formed caller will do (all inputs from
	// the same range file).
	a := makePBF("A", "0-255", [][2]uint32{{1, 10}})
	b := makePBF("B", "256-511", [][2]uint32{{2, 20}})
	out, err := CombineGlyphs("A,B", [][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	_, rangeStr, _ := extractGlyphSummary(t, out)
	if rangeStr != "0-255" {
		t.Errorf("range: got %q, want %q", rangeStr, "0-255")
	}
}

func TestExtractTextFonts(t *testing.T) {
	style := []byte(`{
		"version": 8,
		"layers": [
			{"id": "bg", "type": "background"},
			{"id": "label1", "type": "symbol", "layout": {"text-font": ["Metropolis Regular", "Noto Sans Regular"]}},
			{"id": "label2", "type": "symbol", "layout": {"text-font": ["Metropolis Bold"]}},
			{"id": "label3", "type": "symbol", "layout": {"text-font": ["Metropolis Regular", "Noto Sans Regular"]}},
			{"id": "label4", "type": "symbol", "layout": {"text-font": ["Roboto", "Noto Sans CJK SC Regular"]}},
			{"id": "label5", "type": "symbol", "layout": {"text-font": ["literal", ["Roboto"]]}}
		]
	}`)
	got, err := ExtractTextFonts(style)
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"Metropolis Regular", "Noto Sans Regular"},
		{"Roboto", "Noto Sans CJK SC Regular"},
	}
	// Both ours and `want` are sorted by ExtractTextFonts.
	sort.Slice(want, func(i, j int) bool {
		return strings.Join(want[i], ",") < strings.Join(want[j], ",")
	})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("text-fonts:\n got %v\nwant %v", got, want)
	}
}

func TestEnsureCompositeFontstacks_GeneratesDirectory(t *testing.T) {
	dir := t.TempDir()

	// Constituent fonts with overlapping ranges and overlapping glyph
	// ids. Composite must win-by-first for the shared id.
	mustWrite := func(font, rangeFile string, pairs [][2]uint32) {
		t.Helper()
		fontDir := filepath.Join(dir, font)
		if err := os.MkdirAll(fontDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fontDir, rangeFile),
			makePBF(font, strings.TrimSuffix(rangeFile, ".pbf"), pairs), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite("Metropolis Regular", "0-255.pbf", [][2]uint32{{65, 10}, {66, 10}})
	mustWrite("Metropolis Regular", "256-511.pbf", [][2]uint32{{257, 10}})
	mustWrite("Noto Sans Regular", "0-255.pbf", [][2]uint32{{66, 20}, {67, 20}})
	mustWrite("Noto Sans Regular", "512-767.pbf", [][2]uint32{{513, 20}})

	style := []byte(`{
		"version": 8,
		"layers": [
			{"id": "lbl", "type": "symbol",
			 "layout": {"text-font": ["Metropolis Regular", "Noto Sans Regular"]}}
		]
	}`)
	if err := EnsureCompositeFontstacks(style, dir); err != nil {
		t.Fatal(err)
	}

	composite := filepath.Join(dir, "Metropolis Regular,Noto Sans Regular")
	if _, err := os.Stat(composite); err != nil {
		t.Fatalf("composite dir not created: %v", err)
	}

	check := func(rangeFile string, wantGlyphs map[uint32]uint32) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(composite, rangeFile))
		if err != nil {
			t.Fatalf("read %s: %v", rangeFile, err)
		}
		name, _, glyphs := extractGlyphSummary(t, data)
		if want := "Metropolis Regular,Noto Sans Regular"; name != want {
			t.Errorf("%s name: got %q, want %q", rangeFile, name, want)
		}
		got := map[uint32]uint32{}
		for _, g := range glyphs {
			got[g[0]] = g[1]
		}
		if !reflect.DeepEqual(got, wantGlyphs) {
			t.Errorf("%s glyphs: got %v, want %v", rangeFile, got, wantGlyphs)
		}
	}

	// 0-255: Metropolis covers 65,66 (width 10); Noto adds 67 (width 20);
	// 66 is shared so Metropolis (first) wins.
	check("0-255.pbf", map[uint32]uint32{65: 10, 66: 10, 67: 20})
	// 256-511: only Metropolis has this range.
	check("256-511.pbf", map[uint32]uint32{257: 10})
	// 512-767: only Noto has this range; composite is still produced
	// with the compound name.
	check("512-767.pbf", map[uint32]uint32{513: 20})
}

func TestEnsureCompositeFontstacks_Idempotent(t *testing.T) {
	dir := t.TempDir()
	fontDir := filepath.Join(dir, "Solo Font")
	if err := os.MkdirAll(fontDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fontDir, "0-255.pbf"),
		makePBF("Solo Font", "0-255", [][2]uint32{{1, 1}}), 0o644); err != nil {
		t.Fatal(err)
	}

	style := []byte(`{"layers":[{"id":"x","type":"symbol","layout":{"text-font":["Solo Font","Other"]}}]}`)
	if err := EnsureCompositeFontstacks(style, dir); err != nil {
		t.Fatal(err)
	}

	// Touch a sentinel inside the composite dir; if re-running blows
	// it away the sentinel disappears.
	composite := filepath.Join(dir, "Solo Font,Other")
	if err := os.WriteFile(filepath.Join(composite, "sentinel"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureCompositeFontstacks(style, dir); err != nil {
		t.Fatalf("re-run failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(composite, "sentinel")); err != nil {
		t.Errorf("sentinel was removed; composite is not idempotent: %v", err)
	}
}

func TestEnsureCompositeFontstacks_UnsafeNamesSkipped(t *testing.T) {
	// A style with a path-traversal-shaped constituent must not cause
	// any filesystem operations outside fontsDir. The malicious stack
	// is skipped (logged); a sibling valid stack still processes.
	dir := t.TempDir()
	fontDir := filepath.Join(dir, "Noto Sans Regular")
	if err := os.MkdirAll(fontDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fontDir, "0-255.pbf"),
		makePBF("Noto Sans Regular", "0-255", [][2]uint32{{1, 1}}), 0o644); err != nil {
		t.Fatal(err)
	}
	otherDir := filepath.Join(dir, "Other Font")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "0-255.pbf"),
		makePBF("Other Font", "0-255", [][2]uint32{{2, 2}}), 0o644); err != nil {
		t.Fatal(err)
	}

	style := []byte(`{"layers":[
		{"id":"bad","type":"symbol","layout":{"text-font":["../../etc/passwd","Other Font"]}},
		{"id":"ok","type":"symbol","layout":{"text-font":["Noto Sans Regular","Other Font"]}}
	]}`)
	if err := EnsureCompositeFontstacks(style, dir); err != nil {
		t.Fatal(err)
	}

	// Valid stack still produced.
	if _, err := os.Stat(filepath.Join(dir, "Noto Sans Regular,Other Font", "0-255.pbf")); err != nil {
		t.Errorf("valid sibling stack was not composed: %v", err)
	}
	// Malicious stack did not produce a composite anywhere under dir.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "..") || strings.Contains(e.Name(), "passwd") {
			t.Errorf("path-traversal name reached the filesystem: %q", e.Name())
		}
	}
}

func TestEnsureCompositeFontstacks_MissingFontsSkipped(t *testing.T) {
	dir := t.TempDir()
	// Only Noto Sans installed; Metropolis directory is absent.
	fontDir := filepath.Join(dir, "Noto Sans Regular")
	if err := os.MkdirAll(fontDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fontDir, "0-255.pbf"),
		makePBF("Noto Sans Regular", "0-255", [][2]uint32{{65, 5}}), 0o644); err != nil {
		t.Fatal(err)
	}

	style := []byte(`{"layers":[{"id":"x","type":"symbol","layout":{"text-font":["Metropolis Regular","Noto Sans Regular"]}}]}`)
	if err := EnsureCompositeFontstacks(style, dir); err != nil {
		t.Fatalf("expected success with missing constituent: %v", err)
	}
	composite := filepath.Join(dir, "Metropolis Regular,Noto Sans Regular", "0-255.pbf")
	data, err := os.ReadFile(composite)
	if err != nil {
		t.Fatal(err)
	}
	name, _, glyphs := extractGlyphSummary(t, data)
	if want := "Metropolis Regular,Noto Sans Regular"; name != want {
		t.Errorf("name: got %q, want %q", name, want)
	}
	if len(glyphs) != 1 || glyphs[0] != [2]uint32{65, 5} {
		t.Errorf("glyphs: got %v, want [{65 5}]", glyphs)
	}
}
