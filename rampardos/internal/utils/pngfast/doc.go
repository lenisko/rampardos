// Package pngfast is an in-house fork of Go's image/png writer that
// swaps stdlib compress/zlib for github.com/klauspost/compress/zlib.
// Encode-only — image.Decode via the stdlib decoder is unchanged.
//
// Why a fork (not stdlib): klauspost's flate is ~14% faster than
// stdlib's at equivalent levels, AND produces marginally smaller
// output on our content shape (large multi-staticmap composites).
// stdlib image/png offers no public API to swap the deflate engine.
//
// Why in-house (not the github.com/gameparrot/fastpng third-party
// package this was originally lifted from): fastpng is a drive-by
// fork — three commits, single author, March 2025 — with no
// maintenance signal. The code is byte-for-byte identical to stdlib
// image/png writer.go and paeth.go modulo the two-line zlib swap.
// Owning it directly drops a third-party dep and gives us room to
// add tweaks (e.g. exposing filter mode per
// https://github.com/golang/go/issues/62009) without coordinating
// upstream.
//
// Provenance: writer.go, reader.go, and paeth.go are copied from
// Go's standard library src/image/png/ at commit roughly matching
// Go 1.22+; that code base has been functionally frozen since 2022
// (see commit log before fork). The only edits applied here are:
//
//   - package name: png → pngfast
//   - import: "compress/zlib" → "github.com/klauspost/compress/zlib"
//
// reader.go is included for the constants and types it defines that
// writer.go references (cbG8 / ctGrayscale / nFilter / UnsupportedError
// etc.), not because we need a decoder — image.Decode via stdlib's
// PNG decoder is what rampardos actually uses for reading. Decoder
// code in this package is reachable but inert; harmless dead weight
// vs the alternative of extracting just the shared definitions.
//
// LICENSE in this directory is Go's BSD-3 (the source of the
// material). Track upstream Go changes if any meaningful image/png
// updates land; the historical rate is ~one cosmetic change per year.
package pngfast
