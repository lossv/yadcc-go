// Package compress provides zstd helpers for yadcc inter-daemon transport.
package compress

import (
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

var (
	encoderOnce sync.Once
	encoder     *zstd.Encoder

	decoderOnce sync.Once
	decoder     *zstd.Decoder
)

func getEncoder() *zstd.Encoder {
	encoderOnce.Do(func() {
		var err error
		encoder, err = zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
		if err != nil {
			panic("zstd: create encoder: " + err.Error())
		}
	})
	return encoder
}

func getDecoder() *zstd.Decoder {
	decoderOnce.Do(func() {
		var err error
		decoder, err = zstd.NewReader(nil)
		if err != nil {
			panic("zstd: create decoder: " + err.Error())
		}
	})
	return decoder
}

// Compress returns the zstd-compressed form of src.
func Compress(src []byte) []byte {
	return getEncoder().EncodeAll(src, make([]byte, 0, len(src)/2+256))
}

// Decompress decompresses a zstd-compressed byte slice.
func Decompress(src []byte) ([]byte, error) {
	out, err := getDecoder().DecodeAll(src, make([]byte, 0, len(src)*3))
	if err != nil {
		return nil, fmt.Errorf("zstd decompress: %w", err)
	}
	return out, nil
}
