package lzss_v2

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/klauspost/compress/s2"
	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/require"
)

func testCompressionRoundTrip(t *testing.T, d []byte) {
	dict := []byte{0, 0, 0, 0}
	compressor, err := NewCompressor(dict)
	require.NoError(t, err)

	c, err := compressor.Compress(d)
	require.NoError(t, err)

	dBack, err := Decompress(c, dict)
	require.NoError(t, err)

	if !bytes.Equal(d, dBack) {
		t.Fatal("round trip failed")
	}
}

func Test8Zeros(t *testing.T) {
	testCompressionRoundTrip(t, []byte{0, 0, 0, 0, 0, 0, 0, 0})
}

func Test300Zeros(t *testing.T) { // probably won't happen in our calldata
	testCompressionRoundTrip(t, make([]byte, 300))
}

func TestNoCompression(t *testing.T) {
	testCompressionRoundTrip(t, []byte{'h', 'i'})
}

func Test9E(t *testing.T) {
	testCompressionRoundTrip(t, []byte{1, 1, 1, 1, 2, 1, 1, 1, 1})
}

func Test8ZerosAfterNonzero(t *testing.T) { // probably won't happen in our calldata
	testCompressionRoundTrip(t, append([]byte{1}, make([]byte, 8)...))
}

// Fuzz test the compression / decompression
func FuzzCompress(f *testing.F) {
	f.Add([]byte("hi"))
	f.Add([]byte{0, 0, 0, 0, 0, 0, 0, 0})

	dict := []byte{0, 0, 0, 0}
	compressor, err := NewCompressor(dict)
	if err != nil {
		panic(err)
	}
	f.Fuzz(func(t *testing.T, a []byte) {
		if len(a) > maxInputSize {
			t.Skip("input too large")
		}
		compressedBytes, err := compressor.Compress(a)

		if err != nil {
			t.Fatal(err)
		}

		decompressedBytes, err := Decompress(compressedBytes, dict)

		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Equal(a, decompressedBytes) {
			t.Fatal("decompressed bytes are not equal to original bytes")
		}

	})
}

func Test300ZerosAfterNonzero(t *testing.T) { // probably won't happen in our calldata
	testCompressionRoundTrip(t, append([]byte{'h', 'i'}, make([]byte, 300)...))
}

func TestRepeatedNonzero(t *testing.T) {
	testCompressionRoundTrip(t, []byte{'h', 'i', 'h', 'i', 'h', 'i'})
}

func TestAverageBatch(t *testing.T) {
	assert := require.New(t)

	// read "average_block.hex" file
	d, err := os.ReadFile("./average_block.hex")
	assert.NoError(err)

	// convert to bytes
	data, err := hex.DecodeString(string(d))
	assert.NoError(err)

	dict := getDictionnary()
	compressor, err := NewCompressor(dict)
	assert.NoError(err)
	// test compress round trip with s2, zstd and lzss
	s2Res, err := compressWithS2(data)
	assert.NoError(err)

	zstdRes, err := compressWithZstd(data)
	assert.NoError(err)

	lzssRes, err := compresslzss_v1(compressor, data)
	assert.NoError(err)

	fmt.Println("s2 compression ratio:", s2Res.ratio)
	fmt.Println("zstd compression ratio:", zstdRes.ratio)
	fmt.Println("lzss compression ratio:", lzssRes.ratio)

	// assert.Equal(5.241485472387916, lzssRes.ratio, "regression check")

	// // test decompress round trip with s2, zstd and lzss
	s2Decompressed, err := decompressWithS2(s2Res.compressed)
	assert.NoError(err)

	zstdDecompressed, err := decompressWithZstd(zstdRes.compressed)
	assert.NoError(err)

	lzssDecompressed, err := decompresslzss_v1(lzssRes.compressed, dict)
	assert.NoError(err)

	assert.True(bytes.Equal(data, s2Decompressed))
	assert.True(bytes.Equal(data, zstdDecompressed))
	assert.True(bytes.Equal(data, lzssDecompressed))

}

func BenchmarkAverageBatch(b *testing.B) {
	// read the file
	d, err := os.ReadFile("./average_block.hex")
	if err != nil {
		b.Fatal(err)
	}

	// convert to bytes
	data, err := hex.DecodeString(string(d))
	if err != nil {
		b.Fatal(err)
	}

	dict := getDictionnary()

	// benchmark s2
	b.Run("s2", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := compressWithS2(data)
			if err != nil {
				b.Fatal(err)
			}
		}
	})

	// benchmark zstd
	b.Run("zstd", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := compressWithZstd(data)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	compressor, err := NewCompressor(dict)
	if err != nil {
		b.Fatal(err)
	}

	// benchmark lzss
	b.Run("lzss", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_, err := compresslzss_v1(compressor, data)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}

type compressResult struct {
	compressed []byte
	inputSize  int
	outputSize int
	ratio      float64
}

func decompressWithS2(data []byte) ([]byte, error) {
	r := s2.NewReader(bytes.NewReader(data))
	var dst bytes.Buffer
	_, err := io.Copy(&dst, r)
	return dst.Bytes(), err
}

func compressWithS2(data []byte) (compressResult, error) {
	var buf bytes.Buffer
	w := s2.NewWriter(&buf)
	w.Write(data)
	w.Close()

	res := compressResult{
		compressed: make([]byte, buf.Len()),
		inputSize:  len(data),
		outputSize: buf.Len(),
		ratio:      float64(len(data)) / float64(buf.Len()),
	}
	copy(res.compressed, buf.Bytes())
	return res, nil
}

func decompressWithZstd(data []byte) ([]byte, error) {
	r, err := zstd.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	var dst bytes.Buffer
	_, err = io.Copy(&dst, r)
	return dst.Bytes(), err
}

func compressWithZstd(data []byte) (compressResult, error) {
	var buf bytes.Buffer

	w, err := zstd.NewWriter(&buf)
	if err != nil {
		return compressResult{}, err
	}
	w.Write(data)
	w.Close()

	res := compressResult{
		compressed: make([]byte, buf.Len()),
		inputSize:  len(data),
		outputSize: buf.Len(),
		ratio:      float64(len(data)) / float64(buf.Len()),
	}
	copy(res.compressed, buf.Bytes())
	return res, nil
}

func decompresslzss_v1(data, dict []byte) ([]byte, error) {
	return Decompress(data, dict)
}

func compresslzss_v1(compressor *Compressor, data []byte) (compressResult, error) {
	c, err := compressor.Compress(data)
	if err != nil {
		return compressResult{}, err
	}
	return compressResult{
		compressed: c,
		inputSize:  len(data),
		outputSize: len(c),
		ratio:      float64(len(data)) / float64(len(c)),
	}, nil
}

func getDictionnary() []byte {
	// return bytes.Repeat([]byte{0, 0}, 32)
	// read the dictionary from the file
	d, err := os.ReadFile("dict_naive")
	if err != nil {
		panic(err)
	}
	d = append(d, bytes.Repeat([]byte{0, 0}, 8)...)
	return d
}