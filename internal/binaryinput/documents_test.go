package binaryinput

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"ttc/internal/blobcache"
	"ttc/internal/llm"
)

func TestBinaryDocumentOriginalsAndReferences(t *testing.T) {
	data := []byte("%PDF-1.4\noriginal document bytes\n%%EOF\n")
	file := fileBinaryFile(t, data)
	file.MIMEType, file.Bytes = "application/pdf", len(data)
	want := llm.BinaryPayload{Data: data, MIMEType: "application/pdf"}
	if got, err := Resolve(context.Background(), file); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal(got, err)
	}
	if err := os.WriteFile(file.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := Resolve(context.Background(), file); err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("lost cached original", got, err)
	}
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root, "blob-"+file.SHA256+".blob"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	var unavailable *llm.UnavailableBinaryFileError
	if _, err := Resolve(context.Background(), file); err == nil || errors.As(err, &unavailable) {
		t.Fatal("cache corruption was not fatal", err)
	}
}

func TestBinaryDocumentMetadataValidation(t *testing.T) {
	data := []byte("%PDF-1.4\n%%EOF")
	file := fileBinaryFile(t, data)
	for _, mutation := range []func(*llm.BinaryFile){
		func(f *llm.BinaryFile) { f.MIMEType = "application/pdf" },
		func(f *llm.BinaryFile) { f.MIMEType = "application/pdf"; f.Bytes = len(data) + 1 },
		func(f *llm.BinaryFile) { f.MIMEType = "Application/PDF"; f.Bytes = len(data) },
		func(f *llm.BinaryFile) { f.MIMEType = "application/pdf; charset=utf-8"; f.Bytes = len(data) },
		func(f *llm.BinaryFile) { f.Bytes = -1 },
		func(f *llm.BinaryFile) { f.Bytes = blobcache.MaxBytes + 1 },
	} {
		f := file
		mutation(&f)
		var unavailable *llm.UnavailableBinaryFileError
		if _, err := Resolve(context.Background(), f); err == nil || errors.As(err, &unavailable) {
			t.Fatalf("invalid metadata accepted/degraded: %+v: %v", f, err)
		}
	}
	if _, err := Resolve(context.Background(), file); err == nil || !strings.Contains(err.Error(), "require MIME type") {
		t.Fatal("document MIME guessed", err)
	}
}
