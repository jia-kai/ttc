package llm

import "encoding/json"

// InspectionDocument is a provider-neutral, readable view of a native boundary
// payload. Requests use ordered Items; responses, outcomes and unavailable-parser
// notices use Sections. It does not replace the original raw inspection bytes.
type InspectionDocument struct {
	Sections []InspectionSection
	// Items contains ordered, provider-owned request units for item-level deltas.
	// Nil means unavailable or nonrequest; a parsed empty request has a nonnil
	// empty slice. Parsed requests do not also construct whole-request Sections.
	Items []InspectionItem
	State string // Terminal outcome: completed, failed or interrupted; empty for nonterminal/unknown payloads.
}

// InspectionItem pairs one readable request unit with its full native JSON.
// JSON has stable object-key ordering and preserves decoded strings and exact
// number spellings; it is presentation data, not the original wire capture.
type InspectionItem struct {
	Section string // Generic section title: Settings, Instructions, Tool definitions, or Input messages.
	Name    string // Readable name; input names include their absolute source position.
	Hash    string // Lowercase SHA-256 of Section + NUL + JSON; excludes Name.
	JSON    json.RawMessage
	Value   InspectionValue
}

// InspectionSection groups related native fields under a readable title.
type InspectionSection struct {
	Title  string
	Fields []InspectionField
}

// InspectionField names one value within a section or object.
type InspectionField struct {
	Name  string
	Value InspectionValue
}

// InspectionValue describes semantic JSON values, not rendering instructions.
// Kind is one of the InspectionKind constants. Objects use Fields (in stable
// provider-chosen order); arrays use Items (in source order). Strings store
// decoded text in Text; numbers store their exact JSON number spelling, without
// floating-point conversion. Booleans use "true" or "false"; null has empty
// Text. Opaque Text preserves uninterpreted data such as encrypted or binary
// strings. Only the members appropriate to Kind are populated. Providers may
// expose embedded JSON strings as structured values; raw capture retains the
// original encoding. Empty objects and arrays remain distinct from null.
type InspectionValue struct {
	Kind   string
	Text   string
	Fields []InspectionField
	Items  []InspectionValue
}

const (
	// InspectionKindObject identifies named fields.
	InspectionKindObject = "object"
	// InspectionKindArray identifies ordered values.
	InspectionKindArray = "array"
	// InspectionKindString identifies decoded text.
	InspectionKindString = "string"
	// InspectionKindNumber identifies an exact JSON number lexeme.
	InspectionKindNumber = "number"
	// InspectionKindBool identifies a boolean.
	InspectionKindBool = "bool"
	// InspectionKindNull identifies JSON null.
	InspectionKindNull = "null"
	// InspectionKindOpaque identifies uninterpreted text, not nested JSON.
	InspectionKindOpaque = "opaque"
)

// InspectionParser owns the interpretation of provider-native payloads.
// Direction is "request", "response", or "outcome" as in BoundaryEvent.
// Parsing must not modify payload, access transport credentials, or perform I/O.
// Invalid payloads or unsupported directions return an error.
type InspectionParser interface {
	InspectBoundary(direction string, payload json.RawMessage) (InspectionDocument, error)
}
