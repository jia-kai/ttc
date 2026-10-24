package tui

import (
	"fmt"
	"strings"
	"testing"
)

func TestMarkdownCacheReusesAndBoundsDocuments(t *testing.T) {
	var cache markdownCache
	first := cache.parse("**first**")
	if cache.parse("**first**") != first {
		t.Fatal("unchanged source was reparsed")
	}
	for i := range markdownCacheEntries {
		cache.parse(fmt.Sprintf("document %d", i))
	}
	if len(cache.documents) != markdownCacheEntries || len(cache.order) != markdownCacheEntries {
		t.Fatal("unbounded parsed-document count", len(cache.documents), len(cache.order))
	}
	if cache.parse("**first**") == first {
		t.Fatal("oldest source was not evicted")
	}
	large := strings.Repeat("x", markdownCacheBytes/2+1)
	cache.parse(large)
	cache.parse(large + "y")
	if cache.bytes > markdownCacheBytes || len(cache.documents) != 1 {
		t.Fatal("unbounded parsed source bytes", cache.bytes, len(cache.documents))
	}
	oversized := strings.Repeat("z", markdownCacheBytes+1)
	cache.parse(oversized)
	if _, retained := cache.documents[oversized]; retained {
		t.Fatal("oversized source retained in transcript cache")
	}
}

func TestTranscriptAssetReflowReusesParsedSource(t *testing.T) {
	r := tableMathRenderer()
	source := line{text: "before $x$ after", markdown: true}
	r.layout(source, 40)
	first := r.markdown.documents[source.text]
	if first == nil {
		t.Fatal("transcript source was not retained")
	}
	r.revision++
	r.layout(source, 20)
	if r.markdown.documents[source.text] != first {
		t.Fatal("raster/width reflow reparsed unchanged source")
	}
	source.text += " changed"
	r.layout(source, 20)
	if r.markdown.documents[source.text] == first {
		t.Fatal("changed source reused an unrelated syntax tree")
	}
}
