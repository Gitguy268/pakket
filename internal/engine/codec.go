package engine

import (
	"bufio"
	"compress/bzip2"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	dbzip2 "github.com/dsnet/compress/bzip2"
	"github.com/klauspost/compress/gzip"
	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
	"github.com/sorairolake/lzip-go"
	"github.com/ulikunitz/xz"
	"github.com/ulikunitz/xz/lzma"
)

// clampLevel keeps a level in 0..9.
func clampLevel(l int) int {
	if l < 0 {
		return 0
	}
	if l > 9 {
		return 9
	}
	return l
}

// dictFor maps level 0..9 to an LZMA dictionary size (1 MiB .. 64 MiB).
func dictFor(level int) int {
	sizes := []int{1 << 16, 1 << 20, 1 << 21, 1 << 22, 1 << 22, 1 << 23, 1 << 23, 1 << 24, 1 << 25, 1 << 26}
	return sizes[clampLevel(level)]
}

// newEncoder wraps w in the named stream codec at a 0..9 level.
func newEncoder(codec string, w io.Writer, level int) (io.WriteCloser, error) {
	level = clampLevel(level)
	switch codec {
	case "":
		return nopWriteCloser{w}, nil
	case "gzip":
		gl := level
		if gl == 0 {
			gl = gzip.NoCompression
		}
		return gzip.NewWriterLevel(w, gl)
	case "bzip2":
		bl := level
		if bl < 1 {
			bl = 1
		}
		return dbzip2.NewWriter(w, &dbzip2.WriterConfig{Level: bl})
	case "xz":
		cfg := xz.WriterConfig{DictCap: dictFor(level), CheckSum: xz.CRC64}
		return cfg.NewWriter(w)
	case "zstd":
		el := zstd.SpeedDefault
		switch {
		case level <= 2:
			el = zstd.SpeedFastest
		case level <= 5:
			el = zstd.SpeedDefault
		case level <= 7:
			el = zstd.SpeedBetterCompression
		default:
			el = zstd.SpeedBestCompression
		}
		return zstd.NewWriter(w, zstd.WithEncoderLevel(el), zstd.WithEncoderCRC(true))
	case "brotli":
		bl := level
		if level == 9 {
			bl = 11
		}
		return brotli.NewWriterLevel(w, bl), nil
	case "lz4":
		zw := lz4.NewWriter(w)
		lv := []lz4.CompressionLevel{lz4.Fast, lz4.Level1, lz4.Level2, lz4.Level3, lz4.Level4, lz4.Level5, lz4.Level6, lz4.Level7, lz4.Level8, lz4.Level9}
		if err := zw.Apply(lz4.CompressionLevelOption(lv[level]), lz4.ChecksumOption(true)); err != nil {
			return nil, err
		}
		return zw, nil
	case "lzip":
		return lzip.NewWriterOptions(w, &lzip.WriterOptions{DictSize: uint32(dictFor(level))})
	case "lzma":
		cfg := lzma.WriterConfig{DictCap: dictFor(level), SizeInHeader: false, EOSMarker: true}
		return cfg.NewWriter(w)
	}
	return nil, fmt.Errorf("unknown codec %q", codec)
}

// newDecoder wraps r in the named stream codec.
func newDecoder(codec string, r io.Reader) (io.ReadCloser, error) {
	switch codec {
	case "":
		return io.NopCloser(r), nil
	case "gzip":
		zr, err := gzip.NewReader(bufio.NewReader(r))
		if err != nil {
			return nil, err
		}
		return zr, nil
	case "bzip2":
		return io.NopCloser(bzip2.NewReader(r)), nil
	case "xz":
		zr, err := xz.NewReader(bufio.NewReader(r))
		if err != nil {
			return nil, err
		}
		return io.NopCloser(zr), nil
	case "zstd":
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, err
		}
		return zr.IOReadCloser(), nil
	case "brotli":
		return io.NopCloser(brotli.NewReader(r)), nil
	case "lz4":
		return io.NopCloser(eofReader{lz4.NewReader(r)}), nil
	case "lzip":
		zr, err := lzip.NewReader(bufio.NewReader(r))
		if err != nil {
			return nil, err
		}
		return io.NopCloser(zr), nil
	case "lzma":
		zr, err := lzma.NewReader(bufio.NewReader(r))
		if err != nil {
			return nil, err
		}
		return io.NopCloser(zr), nil
	}
	return nil, fmt.Errorf("unknown codec %q", codec)
}

// eofReader turns the lz4 package's own "EOF" error value, returned when it
// is read again after the end, into io.EOF.
type eofReader struct{ r io.Reader }

func (e eofReader) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && err != io.EOF && err.Error() == "EOF" {
		err = io.EOF
	}
	return n, err
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
