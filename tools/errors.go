package tools

import (
	"encoding/json"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// failed turns a failure into a tool error result carrying doc-1's error shape,
// {"error": {"code": ..., "message": ..., "details": {}}}, with no content
// beside it.
//
// The SDK's own handling would reduce the error to its Error() string, which
// drops the code a client is supposed to branch on and the details an answer is
// made of — for a SYMBOL_AMBIGUOUS that is the candidates, for a SOURCE_MISMATCH
// the two revisions showing which side is stale. So the envelope is written
// here instead. The code is passed through untouched: this function classifies
// nothing. Anything that is not a *[vacerr.Error] has no code to report and is
// handed back to the SDK unchanged.
func failed(err error) (*mcp.CallToolResult, any, error) {
	var vErr *vacerr.Error
	if !errors.As(err, &vErr) {
		return nil, nil, err
	}
	body, marshalErr := json.Marshal(vErr)
	if marshalErr != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}, nil, nil
}
