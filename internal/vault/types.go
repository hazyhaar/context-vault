// CLAUDE:SUMMARY Vault result types — MCP-independent return types for vault operations.
// CLAUDE:DEPENDS (none)
// CLAUDE:EXPORTS Result, Content
package vault

// Content is a single text block in a vault operation result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// Result is the outcome of a vault operation.
type Result struct {
	Content []Content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

// TextResult creates a success Result with a single text content.
func TextResult(text string) Result {
	return Result{Content: []Content{{Type: "text", Text: text}}}
}

// ErrorResult creates an error Result with a single text content.
func ErrorResult(text string) Result {
	return Result{Content: []Content{{Type: "text", Text: text}}, IsError: true}
}
