package graphics

import (
	"bytes"
	"image"
	"reflect"
	"strings"
	"testing"
)

func TestKittyRefreshUploadsVisibleAssetsOnceWithStableIDs(t *testing.T) {
	for _, tmux := range []bool{false, true} {
		var out bytes.Buffer
		g := New(&out, tmux)
		m := image.NewNRGBA(image.Rect(0, 0, 4, 4))
		g.Begin()
		rows, err := g.Place("visible", m, 2, 2, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := g.Place("leaving", m, 2, 2, 1); err != nil {
			t.Fatal(err)
		}
		if err := g.End(); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		g.Refresh()
		if out.Len() != 0 {
			t.Fatal("refresh emitted terminal commands before frame")
		}
		g.Begin()
		again, err := g.Place("visible", m, 2, 2, 1)
		if err != nil || !reflect.DeepEqual(rows, again) {
			t.Fatal("refresh changed placeholders", err)
		}
		if err := g.End(); err != nil {
			t.Fatal(err)
		}
		wire := out.String()
		if strings.Count(wire, "a=T") != 1 || strings.Count(wire, "d=I") != 1 || strings.Contains(wire, "d=A") {
			t.Fatal("refresh did not bound uploads/deletes to owned viewport", wire)
		}
		if len(g.images) != 1 || len(g.stale) != 0 {
			t.Fatal("refresh retained offscreen assets", g.images, g.stale)
		}
		out.Reset()
		g.Begin()
		if _, err := g.Place("visible", m, 2, 2, 1); err != nil {
			t.Fatal(err)
		}
		if err := g.End(); err != nil {
			t.Fatal(err)
		}
		if out.Len() != 0 {
			t.Fatal("idle frame retransmitted pixels")
		}
		g.Refresh()
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
		if len(g.images) != 0 || len(g.stale) != 0 || strings.Count(out.String(), "d=I") != 1 {
			t.Fatal("stale owned placement leaked on close")
		}
	}
}
