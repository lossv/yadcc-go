package protocol

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

func EncodeMultiChunk(parts ...[]byte) []byte {
	lengths := make([]string, 0, len(parts))
	total := 0
	for _, part := range parts {
		lengths = append(lengths, strconv.Itoa(len(part)))
		total += len(part)
	}
	header := strings.Join(lengths, ",") + "\r\n"
	out := make([]byte, 0, len(header)+total)
	out = append(out, header...)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}

func DecodeMultiChunk(data []byte) ([][]byte, error) {
	header, body, ok := bytes.Cut(data, []byte("\r\n"))
	if !ok {
		return nil, fmt.Errorf("missing multichunk header terminator")
	}
	fields := strings.Split(string(header), ",")
	parts := make([][]byte, 0, len(fields))
	offset := 0
	for _, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid multichunk size %q", field)
		}
		if offset+n > len(body) {
			return nil, fmt.Errorf("multichunk body is shorter than declared")
		}
		parts = append(parts, body[offset:offset+n])
		offset += n
	}
	if offset != len(body) {
		return nil, fmt.Errorf("multichunk body has trailing bytes")
	}
	return parts, nil
}
