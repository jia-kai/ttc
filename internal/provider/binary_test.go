package provider

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ttc/internal/blobcache"
)

func TestBinaryDocumentOriginalsAndReferences(t *testing.T) {
	data := []byte("%PDF-1.4\noriginal document bytes\n%%EOF\n")
	file := fileBinaryFile(t, data)
	file.MIMEType, file.Bytes = "application/pdf", len(data)
	want := "data:application/pdf;base64," + base64.StdEncoding.EncodeToString(data)
	if got, err := file.URL(context.Background()); err != nil || got != want {
		t.Fatal(got, err)
	}
	if err := os.WriteFile(file.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := file.URL(context.Background()); err != nil || got != want {
		t.Fatal("lost cached original", got, err)
	}
	cache, err := blobcache.Default()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root, "blob-"+file.SHA256+".blob"), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	var unavailable *UnavailableBinaryFileError
	if _, err := file.URL(context.Background()); err == nil || errors.As(err, &unavailable) {
		t.Fatal("cache corruption was not fatal", err)
	}
}

func TestBinaryDocumentMetadataValidation(t *testing.T) {
	data := []byte("%PDF-1.4\n%%EOF")
	file := fileBinaryFile(t, data)
	for _, mutation := range []func(*BinaryFile){
		func(f *BinaryFile) { f.MIMEType = "application/pdf" },
		func(f *BinaryFile) { f.MIMEType = "application/pdf"; f.Bytes = len(data) + 1 },
		func(f *BinaryFile) { f.MIMEType = "Application/PDF"; f.Bytes = len(data) },
		func(f *BinaryFile) { f.MIMEType = "application/pdf; charset=utf-8"; f.Bytes = len(data) },
		func(f *BinaryFile) { f.Bytes = -1 },
		func(f *BinaryFile) { f.Bytes = blobcache.MaxBytes + 1 },
	} {
		f := file
		mutation(&f)
		var unavailable *UnavailableBinaryFileError
		if _, err := f.URL(context.Background()); err == nil || errors.As(err, &unavailable) {
			t.Fatalf("invalid metadata accepted/degraded: %+v: %v", f, err)
		}
	}
	if _, err := file.URL(context.Background()); err == nil || !strings.Contains(err.Error(), "require MIME type") {
		t.Fatal("document MIME guessed", err)
	}
	for _, url := range []string{"https://example.com/a.pdf", "data:;base64,YQ==", "data:application/pdf,YQ==", "data:application/pdf;base64,?", "data:application/pdf;base64,", "data:application/pdf;base64,YQ==\n"} {
		if _, err := (BinaryFile{DataURL: url}).URL(context.Background()); err == nil {
			t.Fatal("invalid URL accepted", url)
		}
	}
	url := "data:application/pdf;base64,YQ=="
	for _, f := range []BinaryFile{{DataURL: url, MIMEType: "image/png"}, {DataURL: url, Bytes: 2}} {
		if _, err := f.URL(context.Background()); err == nil {
			t.Fatal("inline metadata mismatch accepted", f)
		}
	}
}

func TestBinaryCapabilitiesAndEstimates(t *testing.T) {
	doc := BinaryFileType{MIMEType: "application/pdf", Extensions: []string{".pdf"}, Kind: "document", MaxBytes: 100}
	m := ModelSpec{BinaryFiles: []BinaryFileType{doc}}
	if got := m.BinaryFileTypes(); len(got) != 1 || got[0].MIMEType != doc.MIMEType {
		t.Fatal(got)
	}
	m.Images = true
	formats := m.BinaryFileTypes()
	if len(formats) != 4 {
		t.Fatal(formats)
	}
	formats[3].Extensions[0] = ".mutated"
	if m.BinaryFiles[0].Extensions[0] != ".pdf" {
		t.Fatal("mutable catalog escaped")
	}
	for _, tt := range []struct {
		f    BinaryFile
		want int
	}{
		{BinaryFile{}, 4096},
		{BinaryFile{MIMEType: "image/png", Bytes: 99999}, 4096},
		{BinaryFile{MIMEType: "application/pdf", Bytes: 5000}, 5000},
		{BinaryFile{DataURL: "data:application/pdf;base64,YQ=="}, 4096},
	} {
		if got := tt.f.EstimatedTokens(); got != tt.want {
			t.Fatal(tt, got)
		}
	}
}
