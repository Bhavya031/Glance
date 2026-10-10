package glance

import (
	"container/heap"
	"fmt"
	"sync"

	"github.com/dlclark/regexp2"
	"github.com/tiktoken-go/tokenizer"
)

// Keep the v0.4.0 patterns and mergeable vocabularies; special-token spellings
// remain ordinary text, as they were with Codec.Encode.
const cl100kPattern = `(?i:'s|'t|'re|'ve|'m|'ll|'d)|[^\r\n\p{L}\p{N}]?\p{L}+|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n]*|\s*[\r\n]+|\s+(?!\S)|\s+`
const o200kPattern = `[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]*[\p{Ll}\p{Lm}\p{Lo}\p{M}]+(?i:'s|'t|'re|'ve|'m|'ll|'d)?|[^\r\n\p{L}\p{N}]?[\p{Lu}\p{Lt}\p{Lm}\p{Lo}\p{M}]+[\p{Ll}\p{Lm}\p{Lo}\p{M}]*(?i:'s|'t|'re|'ve|'m|'ll|'d)?|\p{N}{1,3}| ?[^\s\p{L}\p{N}]+[\r\n/]*|\s*[\r\n]+|\s+(?!\S)|\s+`

type tokenCounter struct {
	ranks map[string]uint
	split *regexp2.Regexp
}

var counters = map[string]func() (*tokenCounter, error){
	"cl100k_base": sync.OnceValues(func() (*tokenCounter, error) {
		return newTokenCounter(tokenizer.Cl100kBase, cl100kPattern, 100256)
	}),
	"o200k_base": sync.OnceValues(func() (*tokenCounter, error) {
		return newTokenCounter(tokenizer.O200kBase, o200kPattern, 199998)
	}),
}

func newTokenCounter(name tokenizer.Encoding, pattern string, size uint) (*tokenCounter, error) {
	codec, err := tokenizer.Get(name)
	if err != nil {
		return nil, err
	}
	// The noncapturing wrapper bypasses v0.4.0's incorrectly generated engines
	// registered for the unwrapped patterns, without changing the regex itself.
	split, err := regexp2.Compile("(?:"+pattern+")", regexp2.None)
	if err != nil {
		return nil, err
	}
	ranks := make(map[string]uint, size)
	// v0.4.0's mergeable IDs are contiguous; use its public decoder instead of
	// copying generated vocabularies or relying on private codec fields.
	for id := uint(0); id < size; id++ {
		piece, err := codec.Decode([]uint{id})
		if err != nil {
			return nil, fmt.Errorf("load mergeable token %d: %w", id, err)
		}
		ranks[piece] = id
	}
	return &tokenCounter{ranks: ranks, split: split}, nil
}

func CountTokens(text, encoding string) (int, error) {
	load, ok := counters[encoding]
	if !ok {
		return 0, fmt.Errorf("unsupported encoding %q; supported encodings are o200k_base and cl100k_base", encoding)
	}
	counter, err := load()
	if err != nil {
		return 0, fmt.Errorf("initialize %s tokenizer: %w", encoding, err)
	}
	count := 0
	match, err := counter.split.FindStringMatch(text)
	for match != nil && err == nil {
		count += counter.countPiece(match.String())
		match, err = counter.split.FindNextMatch(match)
	}
	if err != nil {
		return 0, fmt.Errorf("encode with %s: %w", encoding, err)
	}
	return count, nil
}

// countPiece performs the same ranked, leftmost-first BPE merges as upstream,
// with linked byte boundaries and a heap instead of rescanning/shifting a slice.
func (c *tokenCounter) countPiece(piece string) int {
	if _, ok := c.ranks[piece]; ok {
		return 1
	}
	n := len(piece)
	nodes := make([]merge, n+1)
	pairs := make(mergeHeap, 0, n)
	for i := range nodes {
		nodes[i] = merge{offset: i, prev: i - 1, next: i + 1, position: -1}
	}
	update := func(left int) {
		if left < 0 {
			return
		}
		pair := &nodes[left]
		if pair.next < n {
			if rank, ok := c.ranks[piece[left:nodes[pair.next].next]]; ok {
				pair.rank = rank
				if pair.position < 0 {
					heap.Push(&pairs, pair)
				} else {
					heap.Fix(&pairs, pair.position)
				}
				return
			}
		}
		if pair.position >= 0 {
			heap.Remove(&pairs, pair.position)
		}
	}
	for i := 0; i < n-1; i++ {
		update(i)
	}
	count := n
	for len(pairs) > 0 {
		pair := heap.Pop(&pairs).(*merge)
		right := &nodes[pair.next]
		if right.position >= 0 {
			heap.Remove(&pairs, right.position)
		}
		pair.next = right.next
		nodes[right.next].prev = pair.offset
		count--
		update(pair.prev)
		update(pair.offset)
	}
	return count
}

type merge struct {
	rank                         uint
	offset, prev, next, position int
}

type mergeHeap []*merge

func (h mergeHeap) Len() int { return len(h) }
func (h mergeHeap) Less(i, j int) bool {
	return h[i].rank < h[j].rank || h[i].rank == h[j].rank && h[i].offset < h[j].offset
}
func (h mergeHeap) Swap(i, j int) {
	h[i], h[j] = h[j], h[i]
	h[i].position, h[j].position = i, j
}
func (h *mergeHeap) Push(x any) {
	pair := x.(*merge)
	pair.position = len(*h)
	*h = append(*h, pair)
}
func (h *mergeHeap) Pop() any {
	last := len(*h) - 1
	pair := (*h)[last]
	pair.position = -1
	(*h)[last] = nil
	*h = (*h)[:last]
	return pair
}
