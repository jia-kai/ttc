package openai

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestInspectionOutcomeStates(t *testing.T) {
	for _, tc := range []struct {
		raw, state string
	}{
		{`{"status":"success","category":"completed"}`, "completed"},
		{`{"status":"failure","category":"response_error"}`, "failed"},
		{`{"status":"failure","category":"canceled"}`, "interrupted"},
		{`{"status":"unknown"}`, ""},
	} {
		doc, err := new(Adapter).InspectBoundary("outcome", json.RawMessage(tc.raw))
		if err != nil || doc.State != tc.state {
			t.Fatalf("%s: state=%q err=%v", tc.raw, doc.State, err)
		}
	}
}

func TestInspectionEmptyListsRemainExplicit(t *testing.T) {
	doc, err := new(Adapter).InspectBoundary("request", json.RawMessage(`{"input":[],"tools":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Items) != 2 {
		t.Fatalf("empty collections lost native items: %+v", doc.Items)
	}
	for i, key := range []string{"tools", "input"} {
		item := doc.Items[i]
		if item.Name != key || string(item.JSON) != `{"`+key+`":[]}` || item.Hash == "" || item.Value.Kind != "array" || item.Value.Items == nil || len(item.Value.Items) != 0 {
			t.Fatalf("invalid empty collection item: %+v JSON=%s", item, item.JSON)
		}
		if item.Section != []string{"Tool definitions", "Input messages"}[i] {
			t.Fatalf("unexpected collection section: %+v", item)
		}
	}
	if doc.Sections != nil {
		t.Fatal("request constructed redundant sections")
	}
}

func TestInspectionEmptyCollectionTransitions(t *testing.T) {
	parse := func(raw string) []string {
		t.Helper()
		doc, err := new(Adapter).InspectBoundary("request", json.RawMessage(raw))
		if err != nil || doc.Items == nil {
			t.Fatal(doc, err)
		}
		hashes := make([]string, 0, len(doc.Items))
		for _, item := range doc.Items {
			hashes = append(hashes, item.Hash)
		}
		return hashes
	}
	absent := parse(`{"model":"m"}`)
	empty := parse(`{"model":"m","input":[],"tools":[]}`)
	if len(absent) != 1 || len(empty) != 3 || absent[0] != empty[0] || empty[1] == empty[2] {
		t.Fatal(absent, empty)
	}
	if !reflect.DeepEqual(parse(`{"input":[],"tools":[],"model":"m"}`), empty) || !reflect.DeepEqual(parse(`{"model":"m"}`), absent) {
		t.Fatal("collection hashes are unstable")
	}
	// Even native array elements that are themselves empty arrays must not
	// collide with the wrapped marker for an explicitly empty collection.
	nonempty := parse(`{"model":"m","input":[[]],"tools":[[]]}`)
	if len(nonempty) != 3 || empty[1] == nonempty[1] || empty[2] == nonempty[2] {
		t.Fatal(empty, nonempty)
	}
}
