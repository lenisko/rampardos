package renderer

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/lenisko/rampardos/internal/services"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/encoding/protowire"
)

// Mapbox glyph PBF wire schema (paraphrased from the upstream .proto):
//
//	message glyphs    { repeated fontstack stacks = 1; }
//	message fontstack { required string name = 1; required string range = 2;
//	                    repeated glyph glyphs = 3; }
//	message glyph     { required uint32 id = 1; optional bytes bitmap = 2;
//	                    required uint32 width = 3; required uint32 height = 4;
//	                    required sint32 left  = 5; required sint32 top    = 6;
//	                    required uint32 advance = 7; }
//
// The merge operates at the wire level using google.golang.org/protobuf's
// protowire package — already a transitive dep via prometheus/client_golang,
// so no new go.mod entries. Each glyph entry from the inputs is kept as
// raw bytes; only the id field is read for dedup. The output is wrapped
// in a fresh glyphs/fontstack with the compound name. No per-field
// decode/encode of glyph bodies.
const (
	fieldGlyphsStacks   = 1
	fieldFontstackName  = 1
	fieldFontstackRange = 2
	fieldFontstackGlyph = 3
	fieldGlyphID        = 1
	fieldGlyphWidth     = 3 // used in tests; declared here so production and tests share the schema constants
)

// composeWorkers bounds parallelism within a single composeOneFontstack
// call. The work is mostly disk I/O — reading a handful of small PBFs
// per range and writing one — so a small fan-out is enough to overlap
// kernel work without thrashing.
const composeWorkers = 8

// CombineGlyphs merges multiple glyph PBF buffers into a single PBF
// labelled with the compound fontstack name. Inputs are expected to
// share a range (e.g. "0-255"); the range is taken from the first
// input that supplies one. Glyphs are deduplicated by id and the
// earliest input wins — standard MapLibre fontstack fallback, where
// the first font in the stack covers what it can and later fonts
// fill the gaps.
//
// Callers may pass a single input; the function still rewrites the
// fontstack name to the compound form so maplibre-native accepts the
// response for the requested URL.
func CombineGlyphs(compoundName string, inputs [][]byte) ([]byte, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("no inputs")
	}
	seen := make(map[uint32]struct{})
	var (
		entries  [][]byte
		rangeStr string
	)
	for i, in := range inputs {
		_, r, glyphs, err := parseGlyphsPBF(in)
		if err != nil {
			return nil, fmt.Errorf("input %d: %w", i, err)
		}
		if rangeStr == "" {
			rangeStr = r
		}
		for _, g := range glyphs {
			if _, ok := seen[g.id]; ok {
				continue
			}
			seen[g.id] = struct{}{}
			entries = append(entries, g.body)
		}
	}
	if rangeStr == "" {
		return nil, fmt.Errorf("no range field in any input")
	}
	return encodeGlyphsPBF(compoundName, rangeStr, entries), nil
}

type rawGlyph struct {
	id   uint32
	body []byte // sub-slice of the input buffer — no copy
}

// parseGlyphsPBF walks one glyphs PBF and returns the fontstack name,
// range string, and glyph entries (as raw bytes paired with their
// ids). Multi-stack inputs are flattened: glyphs from every stack
// are appended in order, and the first non-empty name/range wins.
func parseGlyphsPBF(data []byte) (name, rangeStr string, glyphs []rawGlyph, err error) {
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if perr := protowire.ParseError(n); perr != nil {
			return "", "", nil, perr
		}
		data = data[n:]
		if num != fieldGlyphsStacks || typ != protowire.BytesType {
			skip := protowire.ConsumeFieldValue(num, typ, data)
			if perr := protowire.ParseError(skip); perr != nil {
				return "", "", nil, perr
			}
			data = data[skip:]
			continue
		}
		stack, sn := protowire.ConsumeBytes(data)
		if perr := protowire.ParseError(sn); perr != nil {
			return "", "", nil, perr
		}
		data = data[sn:]
		for len(stack) > 0 {
			inNum, inTyp, inN := protowire.ConsumeTag(stack)
			if perr := protowire.ParseError(inN); perr != nil {
				return "", "", nil, perr
			}
			stack = stack[inN:]
			switch {
			case inNum == fieldFontstackName && inTyp == protowire.BytesType:
				v, vn := protowire.ConsumeBytes(stack)
				if perr := protowire.ParseError(vn); perr != nil {
					return "", "", nil, perr
				}
				if name == "" {
					name = string(v)
				}
				stack = stack[vn:]
			case inNum == fieldFontstackRange && inTyp == protowire.BytesType:
				v, vn := protowire.ConsumeBytes(stack)
				if perr := protowire.ParseError(vn); perr != nil {
					return "", "", nil, perr
				}
				if rangeStr == "" {
					rangeStr = string(v)
				}
				stack = stack[vn:]
			case inNum == fieldFontstackGlyph && inTyp == protowire.BytesType:
				v, vn := protowire.ConsumeBytes(stack)
				if perr := protowire.ParseError(vn); perr != nil {
					return "", "", nil, perr
				}
				stack = stack[vn:]
				id, perr := extractGlyphID(v)
				if perr != nil {
					return "", "", nil, fmt.Errorf("glyph id: %w", perr)
				}
				glyphs = append(glyphs, rawGlyph{id: id, body: v})
			default:
				skip := protowire.ConsumeFieldValue(inNum, inTyp, stack)
				if perr := protowire.ParseError(skip); perr != nil {
					return "", "", nil, perr
				}
				stack = stack[skip:]
			}
		}
	}
	return name, rangeStr, glyphs, nil
}

// extractGlyphID scans a glyph message for its id field. The schema
// lists id as field 1, but proto encoders aren't required to emit
// fields in number order — scan defensively rather than assume.
func extractGlyphID(glyph []byte) (uint32, error) {
	data := glyph
	for len(data) > 0 {
		num, typ, n := protowire.ConsumeTag(data)
		if perr := protowire.ParseError(n); perr != nil {
			return 0, perr
		}
		data = data[n:]
		if num == fieldGlyphID && typ == protowire.VarintType {
			v, vn := protowire.ConsumeVarint(data)
			if perr := protowire.ParseError(vn); perr != nil {
				return 0, perr
			}
			return uint32(v), nil
		}
		skip := protowire.ConsumeFieldValue(num, typ, data)
		if perr := protowire.ParseError(skip); perr != nil {
			return 0, perr
		}
		data = data[skip:]
	}
	return 0, fmt.Errorf("glyph entry missing required id field")
}

func encodeGlyphsPBF(name, rangeStr string, glyphBodies [][]byte) []byte {
	var stack []byte
	stack = protowire.AppendTag(stack, fieldFontstackName, protowire.BytesType)
	stack = protowire.AppendString(stack, name)
	stack = protowire.AppendTag(stack, fieldFontstackRange, protowire.BytesType)
	stack = protowire.AppendString(stack, rangeStr)
	for _, body := range glyphBodies {
		stack = protowire.AppendTag(stack, fieldFontstackGlyph, protowire.BytesType)
		stack = protowire.AppendBytes(stack, body)
	}
	var out []byte
	out = protowire.AppendTag(out, fieldGlyphsStacks, protowire.BytesType)
	out = protowire.AppendBytes(out, stack)
	return out
}

var glyphRangeRe = regexp.MustCompile(`^\d+-\d+\.pbf$`)

// ExtractTextFonts returns the deduplicated, sorted set of compound
// fontstacks (length > 1) referenced via `layout.text-font` in the
// style. Expression-form `text-font` values (e.g. `["literal", […]]`,
// `["step", …]`) are ignored: only plain string arrays are composed.
// Operators using expression forms must materialise composites
// manually.
func ExtractTextFonts(styleJSON []byte) ([][]string, error) {
	var style struct {
		Layers []struct {
			Layout map[string]json.RawMessage `json:"layout"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(styleJSON, &style); err != nil {
		return nil, fmt.Errorf("parse style: %w", err)
	}
	seen := make(map[string]struct{})
	var out [][]string
	for _, layer := range style.Layers {
		raw, ok := layer.Layout["text-font"]
		if !ok {
			continue
		}
		var fonts []string
		if err := json.Unmarshal(raw, &fonts); err != nil {
			continue
		}
		if len(fonts) < 2 {
			continue
		}
		key := strings.Join(fonts, ",")
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, fonts)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i], ",") < strings.Join(out[j], ",")
	})
	return out, nil
}

// EnsureCompositeFontstacks materialises composite font directories
// for every compound fontstack referenced by the style, by composing
// each range PBF across the constituent fonts. Idempotent: a target
// directory that already exists is left alone (delete it to force
// regeneration).
//
// Constituent fonts that aren't installed are skipped. A composite
// is still generated as long as at least one constituent contributes;
// the resulting directory has glyphs only from the available fonts,
// which matches MapLibre's natural fallback behaviour.
//
// Stacks containing a font name that would escape fontsDir (path
// traversal in style-supplied input) are logged and skipped rather
// than aborting the whole style load.
func EnsureCompositeFontstacks(styleJSON []byte, fontsDir string) error {
	stacks, err := ExtractTextFonts(styleJSON)
	if err != nil {
		return err
	}
	for _, stack := range stacks {
		if err := composeOneFontstack(fontsDir, stack); err != nil {
			return fmt.Errorf("compose %q: %w", strings.Join(stack, ","), err)
		}
	}
	return nil
}

func composeOneFontstack(fontsDir string, fonts []string) error {
	for _, f := range fonts {
		if _, err := services.SanitizeName(f); err != nil {
			// style-supplied font names that look like path traversal
			// or contain separators are skipped rather than allowed to
			// reach filepath.Join. Don't fail the whole style load —
			// other stacks may still be valid.
			slog.Warn("skipping fontstack with unsafe constituent name",
				"stack", strings.Join(fonts, ","), "constituent", f, "error", err)
			return nil
		}
	}
	compound := strings.Join(fonts, ",")
	target := filepath.Join(fontsDir, compound)
	if _, err := os.Stat(target); err == nil {
		return nil
	}

	rangeSet := make(map[string]struct{})
	for _, f := range fonts {
		entries, err := os.ReadDir(filepath.Join(fontsDir, f))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() && glyphRangeRe.MatchString(e.Name()) {
				rangeSet[e.Name()] = struct{}{}
			}
		}
	}
	if len(rangeSet) == 0 {
		return fmt.Errorf("no constituent fonts installed for compound %q", compound)
	}

	if err := os.MkdirAll(fontsDir, 0o755); err != nil {
		return err
	}
	tmpDir, err := os.MkdirTemp(fontsDir, ".compose-*")
	if err != nil {
		return err
	}

	// Fan out per range: the work is one read per constituent + one
	// compose + one write. Bounded by composeWorkers so we don't
	// thrash the disk on a 100-range Unicode-covering style.
	g := new(errgroup.Group)
	g.SetLimit(composeWorkers)
	for rangeFile := range rangeSet {
		g.Go(func() error {
			return composeOneRange(fontsDir, fonts, compound, rangeFile, tmpDir)
		})
	}
	if err := g.Wait(); err != nil {
		os.RemoveAll(tmpDir)
		return err
	}

	if err := os.Rename(tmpDir, target); err != nil {
		// Two style loads referencing the same compound can race here:
		// both saw the target missing on entry, both composed into
		// their own tempdir, both try to rename. POSIX fails the
		// second rename if the target now exists. Accept that — a
		// sibling produced an equivalent composite — and clean up
		// our loser tempdir.
		os.RemoveAll(tmpDir)
		if _, statErr := os.Stat(target); statErr == nil {
			return nil
		}
		return err
	}
	return nil
}

func composeOneRange(fontsDir string, fonts []string, compound, rangeFile, outDir string) error {
	var inputs [][]byte
	for _, f := range fonts {
		data, err := os.ReadFile(filepath.Join(fontsDir, f, rangeFile))
		if err != nil {
			continue
		}
		inputs = append(inputs, data)
	}
	if len(inputs) == 0 {
		return nil
	}
	out, err := CombineGlyphs(compound, inputs)
	if err != nil {
		return fmt.Errorf("compose %s: %w", rangeFile, err)
	}
	return os.WriteFile(filepath.Join(outDir, rangeFile), out, 0o644)
}
