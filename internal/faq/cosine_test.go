package faq

import (
	"math"
	"testing"

	"github.com/pgvector/pgvector-go"
)

func TestCosineDist(t *testing.T) {
	cases := []struct {
		name string
		a    []float32
		b    []float32
		want float64
		tol  float64
	}{
		{
			name: "identical_vectors_zero_distance",
			a:    []float32{1, 2, 3},
			b:    []float32{1, 2, 3},
			want: 0.0,
			tol:  1e-6,
		},
		{
			name: "orthogonal_vectors_one_distance",
			a:    []float32{1, 0, 0},
			b:    []float32{0, 1, 0},
			want: 1.0,
			tol:  1e-6,
		},
		{
			name: "opposite_vectors_two_distance",
			a:    []float32{1, 1},
			b:    []float32{-1, -1},
			want: 2.0, // cos(180°) = -1 → 1 - (-1) = 2
			tol:  1e-6,
		},
		{
			name: "different_length_one_distance",
			a:    []float32{1, 2, 3},
			b:    []float32{1, 2},
			want: 1.0,
			tol:  0,
		},
		{
			name: "zero_vector_one_distance",
			a:    []float32{0, 0, 0},
			b:    []float32{1, 2, 3},
			want: 1.0,
			tol:  0,
		},
		{
			name: "both_empty_one_distance",
			a:    []float32{},
			b:    []float32{},
			want: 1.0, // длины равны, но нормы нулевые → ветка na==0 → 1.0
			tol:  0,
		},
		{
			name: "close_vectors_small_distance",
			a:    []float32{1, 0, 0},
			b:    []float32{0.99, 0.01, 0},
			want: 0.0,
			tol:  0.22, // строго меньше clusterMaxDist
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cosineDist(c.a, c.b)
			if math.Abs(got-c.want) > c.tol {
				t.Fatalf("cosineDist(%v, %v) = %v, want %v (±%v)", c.a, c.b, got, c.want, c.tol)
			}
		})
	}
}

func TestClusterQuestions(t *testing.T) {
	rows := []questionRow{
		{ID: 1, Text: "как поставить php?", Embed: pgvector.NewVector([]float32{1, 0, 0})},
		{ID: 2, Text: "как установить php?", Embed: pgvector.NewVector([]float32{0.99, 0.01, 0})},
		{ID: 3, Text: "что такое docker?", Embed: pgvector.NewVector([]float32{0, 1, 0})},
	}
	cl := clusterQuestions(rows, clusterMaxDist)
	if len(cl) != 2 {
		t.Fatalf("expected 2 clusters, got %d", len(cl))
	}
}
