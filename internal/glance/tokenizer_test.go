package glance

import (
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
)

func TestHeapMergesMatchLinearBPE(t *testing.T) {
	for encoding, load := range counters {
		t.Run(encoding, func(t *testing.T) {
			t.Parallel()
			counter, err := load()
			if err != nil {
				t.Fatal(err)
			}
			rng := rand.New(rand.NewSource(1))
			alphabet := []byte("   \t\n\rabcABC'!?世界")
			for i := 0; i < 300; i++ {
				piece := make([]byte, 1+rng.Intn(512))
				for j := range piece {
					piece[j] = alphabet[rng.Intn(len(alphabet))]
				}
				if i < 150 {
					piece = []byte(strings.Repeat(" ", i+1))
				}
				text := string(piece)
				if got, want := counter.countPiece(text), linearBPECount(text, counter.ranks); got != want {
					t.Fatalf("%q: heap=%d, linear=%d", text, got, want)
				}
			}
		})
	}
}

// Reference the original rank/leftmost merge rule independently of the heap.
func linearBPECount(piece string, ranks map[string]uint) int {
	if _, ok := ranks[piece]; ok {
		return 1
	}
	parts := make([]string, len(piece))
	for i := range parts {
		parts[i] = piece[i : i+1]
	}
	for {
		index, lowest := -1, ^uint(0)
		for i := 0; i < len(parts)-1; i++ {
			if rank, ok := ranks[parts[i]+parts[i+1]]; ok && rank < lowest {
				index, lowest = i, rank
			}
		}
		if index < 0 {
			return len(parts)
		}
		parts[index] += parts[index+1]
		parts = append(parts[:index+1], parts[index+2:]...)
	}
}

func BenchmarkCountTokensPadding(b *testing.B) {
	for _, encoding := range []string{"o200k_base", "cl100k_base"} {
		for name, text := range map[string]string{
			"spaces": strings.Repeat(" ", 200000),
			"mixed":  strings.Repeat(" \t\n", 50000),
		} {
			b.Run(encoding+"/"+name, func(b *testing.B) {
				if _, err := CountTokens(text, encoding); err != nil {
					b.Fatal(err)
				}
				b.SetBytes(int64(len(text)))
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, err := CountTokens(text, encoding); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

func TestEstimatePaddedContext(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "task.txt"), []byte(strings.Repeat(" ", 200000)))
	for _, encoding := range []string{"o200k_base", "cl100k_base"} {
		e, err := EstimatePath(dir, []byte("task"), "", 0, "", "standard", encoding, DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		// Python tiktoken 0.14.0: Instructions:\ntask\n\nFile: task.txt\n---\n
		// followed by 200,000 spaces and \n---\n\n is 1,575 tokens in both encodings.
		if e.ContextTokens != 1575 || len(e.SelectedFiles) != 1 {
			t.Fatalf("%s: context=%d selected=%d; want 1575 and 1", encoding, e.ContextTokens, len(e.SelectedFiles))
		}
	}
}
