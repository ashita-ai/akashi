package sqlite

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCosineSimilarity_IdenticalVectors(t *testing.T) {
	v := []float32{1, 2, 3, 4, 5}
	assert.InDelta(t, 1.0, cosineSimilarity(v, v), 1e-9)
}

func TestCosineSimilarity_OrthogonalVectors(t *testing.T) {
	a := []float32{1, 0, 0}
	b := []float32{0, 1, 0}
	assert.InDelta(t, 0.0, cosineSimilarity(a, b), 1e-9)
}

func TestCosineSimilarity_OppositeVectors(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{-1, -2, -3}
	assert.InDelta(t, -1.0, cosineSimilarity(a, b), 1e-9)
}

func TestCosineSimilarity_KnownValues(t *testing.T) {
	// Hand-computed: cos(a, b) = (1*4 + 2*5 + 3*6) / (sqrt(14) * sqrt(77))
	a := []float32{1, 2, 3}
	b := []float32{4, 5, 6}
	expected := 32.0 / (math.Sqrt(14) * math.Sqrt(77))
	assert.InDelta(t, expected, cosineSimilarity(a, b), 1e-9)
}

func TestCosineSimilarity_EmptyInput(t *testing.T) {
	assert.Equal(t, 0.0, cosineSimilarity(nil, nil))
	assert.Equal(t, 0.0, cosineSimilarity([]float32{}, []float32{}))
}

func TestCosineSimilarity_MismatchedLengths(t *testing.T) {
	a := []float32{1, 2, 3}
	b := []float32{4, 5}
	assert.Equal(t, 0.0, cosineSimilarity(a, b))
}

func TestCosineSimilarity_ZeroVector(t *testing.T) {
	a := []float32{0, 0, 0}
	b := []float32{1, 2, 3}
	assert.Equal(t, 0.0, cosineSimilarity(a, b))
	assert.Equal(t, 0.0, cosineSimilarity(b, a))
	assert.Equal(t, 0.0, cosineSimilarity(a, a))
}

func TestMarshalUnmarshalFloat32s_RoundTrip(t *testing.T) {
	original := []float32{1.5, -2.7, 0, math.MaxFloat32, math.SmallestNonzeroFloat32}
	blob := marshalFloat32s(original)
	require.NotNil(t, blob)
	assert.Len(t, blob, len(original)*4)

	decoded, err := unmarshalFloat32s(blob)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}

func TestMarshalFloat32s_NilInput(t *testing.T) {
	assert.Nil(t, marshalFloat32s(nil))
	assert.Nil(t, marshalFloat32s([]float32{}))
}

func TestUnmarshalFloat32s_EmptyInput(t *testing.T) {
	v, err := unmarshalFloat32s(nil)
	require.NoError(t, err)
	assert.Nil(t, v)

	v, err = unmarshalFloat32s([]byte{})
	require.NoError(t, err)
	assert.Nil(t, v)
}

func TestUnmarshalFloat32s_InvalidLength(t *testing.T) {
	_, err := unmarshalFloat32s([]byte{1, 2, 3})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not a multiple of 4")
}

func TestMarshalUnmarshalFloat32s_LargeVector(t *testing.T) {
	// Simulate a 1024-dimension embedding.
	original := make([]float32, 1024)
	for i := range original {
		original[i] = float32(i) * 0.001
	}
	blob := marshalFloat32s(original)
	assert.Len(t, blob, 1024*4)

	decoded, err := unmarshalFloat32s(blob)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}
