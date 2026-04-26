package protocol

import (
	"bytes"
	"testing"
)

func TestMultiChunkRoundTrip(t *testing.T) {
	encoded := EncodeMultiChunk([]byte("meta"), []byte("0123456789"), []byte{})

	parts, err := DecodeMultiChunk(encoded)
	if err != nil {
		t.Fatalf("DecodeMultiChunk() error = %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("len(parts) = %d, want 3", len(parts))
	}
	if !bytes.Equal(parts[0], []byte("meta")) {
		t.Fatalf("parts[0] = %q", parts[0])
	}
	if !bytes.Equal(parts[1], []byte("0123456789")) {
		t.Fatalf("parts[1] = %q", parts[1])
	}
	if !bytes.Equal(parts[2], []byte{}) {
		t.Fatalf("parts[2] = %q", parts[2])
	}
}

func TestDecodeMultiChunkRejectsTrailingBytes(t *testing.T) {
	_, err := DecodeMultiChunk([]byte("1\r\nab"))
	if err == nil {
		t.Fatal("DecodeMultiChunk() error = nil, want error")
	}
}

func TestDecodeMultiChunkRejectsShortBody(t *testing.T) {
	_, err := DecodeMultiChunk([]byte("4\r\nabc"))
	if err == nil {
		t.Fatal("DecodeMultiChunk() error = nil, want error")
	}
}
