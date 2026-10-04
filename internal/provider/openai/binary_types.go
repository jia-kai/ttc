package openai

import (
	"ttc/internal/blobcache"
	"ttc/internal/provider"
)

// documentFileTypes follows the binary document MIME/extension catalog at
// https://developers.openai.com/api/docs/guides/file-inputs (2026-10-04).
// Text/code remain paginated text reads. These are public Responses formats;
// announcement does not imply that every subscription endpoint accepts them.
func documentFileTypes() []provider.BinaryFileType {
	formats := []provider.BinaryFileType{
		{MIMEType: "application/pdf", Extensions: []string{".pdf"}},
		{MIMEType: "application/msword", Extensions: []string{".doc", ".dot"}},
		{MIMEType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document", Extensions: []string{".docx"}},
		{MIMEType: "application/vnd.ms-excel", Extensions: []string{".xla", ".xlb", ".xlc", ".xlm", ".xls", ".xlt", ".xlw"}},
		{MIMEType: "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", Extensions: []string{".xlsx"}},
		{MIMEType: "application/vnd.ms-powerpoint", Extensions: []string{".pot", ".ppa", ".pps", ".ppt", ".pwz", ".wiz"}},
		{MIMEType: "application/vnd.openxmlformats-officedocument.presentationml.presentation", Extensions: []string{".pptx"}},
		{MIMEType: "application/rtf", Extensions: []string{".rtf"}},
		{MIMEType: "text/rtf", Extensions: []string{".rtf"}},
		{MIMEType: "application/vnd.oasis.opendocument.text", Extensions: []string{".odt"}},
		{MIMEType: "application/vnd.apple.pages", Extensions: []string{".pages"}},
		{MIMEType: "application/vnd.apple.keynote", Extensions: []string{".key"}},
		{MIMEType: "application/vnd.apple.iwork", Extensions: []string{".pages", ".key"}},
	}
	for i := range formats {
		formats[i].Kind = "document"
		formats[i].MaxBytes = blobcache.MaxBytes
	}
	return formats
}
