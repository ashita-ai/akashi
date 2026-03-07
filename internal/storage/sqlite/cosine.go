package sqlite

import (
	"encoding/binary"
	"fmt"
	"math"
)

// cosineSimilarity computes the cosine similarity between two float32 slices.
// Returns 0 if either vector is zero-length, mismatched in length, or has zero norm.
// The result is in the range [-1, 1] where 1 means identical direction.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		da, db := float64(a[i]), float64(b[i])
		dot += da * db
		normA += da * da
		normB += db * db
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// marshalFloat32s encodes a []float32 slice into a little-endian byte slice.
// Each float32 occupies 4 bytes. Returns nil for nil or empty input.
func marshalFloat32s(v []float32) []byte {
	if len(v) == 0 {
		return nil
	}
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// unmarshalFloat32s decodes a little-endian byte slice into a []float32.
// Returns an error if the byte slice length is not a multiple of 4.
func unmarshalFloat32s(data []byte) ([]float32, error) {
	if len(data) == 0 {
		return nil, nil
	}
	if len(data)%4 != 0 {
		return nil, fmt.Errorf("sqlite: embedding blob length %d is not a multiple of 4", len(data))
	}
	v := make([]float32, len(data)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return v, nil
}
