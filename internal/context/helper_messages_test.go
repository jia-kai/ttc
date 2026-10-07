package context

import (
	"fmt"
	"reflect"
	"testing"

	"ttc/internal/llm"
	"ttc/internal/prompts"
)

func TestAttachmentMessageUsesPromptAssets(t *testing.T) {
	image := llm.BinaryFile{Path: "/image.png", MIMEType: "image/png"}
	document := llm.BinaryFile{Path: "/document.pdf", MIMEType: "application/pdf"}
	input := Input{Text: "Inspect", Source: "queue", Attachments: []Attachment{
		{Path: image.Path, Kind: "image", File: &image},
		{Path: document.Path, Kind: "document", File: &document},
		{Path: "/source.go", Kind: "text", Text: "package sample", Truncated: true},
	}}
	message := input.Message()
	want := "Inspect" + fmt.Sprintf(prompts.AttachmentImage, image.Path) +
		fmt.Sprintf(prompts.AttachmentDocument, document.Path) +
		fmt.Sprintf(prompts.AttachmentText, "text", "/source.go", "package sample") + prompts.AttachmentTruncated
	if message.Content != want || message.InputSource != input.Source || message.UserText == nil || *message.UserText != input.Text || !reflect.DeepEqual(message.Files, []llm.BinaryFile{image, document}) {
		t.Fatalf("attachment metadata or prompt changed: %+v", message)
	}
	if message.Content != "Inspect\nImage attachment: /image.png\nDocument attachment: /document.pdf\n\nAttachment (text): /source.go\npackage sample\n[attachment truncated]" {
		t.Fatalf("attachment scaffold wording changed: %q", message.Content)
	}
}
