package session

import (
	"reflect"
	"testing"
)

func TestModelReselectionAdoptsRefreshedMetadata(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		name := "blank"
		if persisted {
			name = "persisted"
		}
		t.Run(name, func(t *testing.T) {
			r, _ := runtimeFixture(t, nil)
			if persisted {
				seedRuntime(t, r, "existing conversation")
			}
			previous := r.CurrentSelection()
			next := previous
			next.Model.Name = "Refreshed name"
			next.Model.Revision = "fresh"
			next.Model.Budget.ContextLimit += 1000
			if err := r.RequestModel(next); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(r.CurrentSelection(), previous) {
				t.Fatal("selection changed before boundary")
			}
			event, err := r.ApplyModel("")
			if err != nil {
				t.Fatal(err)
			}
			if event.Kind != "status" || !reflect.DeepEqual(r.CurrentSelection(), next) {
				t.Fatal("refreshed metadata not applied", event, r.CurrentSelection())
			}
			if persisted {
				saved, err := r.Store.Session(r.Current())
				if err != nil || !reflect.DeepEqual(saved.Model, next) || event.EntryID == 0 {
					t.Fatal("metadata not persisted", saved.Model, event, err)
				}
			}
			if err := r.RequestModel(next); err != nil {
				t.Fatal(err)
			}
			if event, err := r.ApplyModel(""); err != nil || event.Kind != "" {
				t.Fatal("identical metadata should be a no-op", event, err)
			}
		})
	}
}
