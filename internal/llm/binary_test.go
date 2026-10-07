package llm

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBinaryFileRejectsInlineJSON(t *testing.T) {
	for _, raw := range []string{
		`{"data_url":"data:application/pdf;base64,YQ=="}`,
		`{"data_url":"","path":"/source.pdf","sha256":"checksum"}`,
		`{"data_url":null}`,
		`{"role":"user","files":[{"data_url":"data:image/png;base64,YQ=="}]}`,
	} {
		var target any = &BinaryFile{}
		if strings.Contains(raw, `"files"`) {
			target = &Message{}
		}
		if err := json.Unmarshal([]byte(raw), target); err == nil || !strings.Contains(err.Error(), "inline binary data_url is unsupported") {
			t.Fatalf("obsolete inline input silently accepted: %s: %v", raw, err)
		}
	}
	ref := BinaryFile{Path: "/original.pdf", SHA256: strings.Repeat("a", 64), MIMEType: "application/pdf", Bytes: 13}
	raw, err := json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	var decoded BinaryFile
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != ref {
		t.Fatalf("native reference failed to round-trip: %+v %v", decoded, err)
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
	for _, f := range formats[:3] {
		if f.MaxBytes != 32<<20 {
			t.Fatal("default image byte bound changed", f)
		}
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
		{BinaryFile{MIMEType: "application/pdf", Bytes: 1}, 4096},
	} {
		if got := tt.f.EstimatedTokens(); got != tt.want {
			t.Fatal(tt, got)
		}
	}
}
