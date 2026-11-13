package main

import (
	"flag"
	"fmt"

	"relay/internal/papers"
)

// cmdInspect prints chunk statistics so retrieval settings can be sanity-checked.
func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	c := addCommon(fs)
	fs.Parse(args)
	lib := c.library()
	for _, piece := range []int{120, 220, 450, 900} {
		lib.ChunkAll(piece, 40)
		var total, maxW, over int
		for _, ch := range lib.Chunks {
			total += ch.Words
			if ch.Words > maxW {
				maxW = ch.Words
			}
			if ch.Words > piece*2 {
				over++
			}
		}
		avg := 0
		if len(lib.Chunks) > 0 {
			avg = total / len(lib.Chunks)
		}
		fmt.Printf("piece=%4d chunks=%3d avg_words=%4d max_words=%4d oversized(>2x)=%d\n",
			piece, len(lib.Chunks), avg, maxW, over)
	}
	// Show the actual top chunks for the test query.
	lib.ChunkAll(220, 40)
	for _, ch := range lib.Chunks[:0] {
		_ = ch
	}
	_ = papers.Load
}
