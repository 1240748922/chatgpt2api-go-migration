package provider

import (
	"errors"
	"fmt"
)

var ErrImageTextReply = errors.New("upstream returned terminal text without an image")
var ErrImageGenerationStopped = errors.New("upstream reported an image generation error")

// Submitted includes an uncertain POST outcome. Such errors must resume the
// existing conversation, never trigger another paid generation automatically.
type ImageFlowError struct {
	Stage          string
	ConversationID string
	Submitted      bool
	Cause          error
}

func (e *ImageFlowError) Error() string { return fmt.Sprintf("image %s failed: %v", e.Stage, e.Cause) }
func (e *ImageFlowError) Unwrap() error { return e.Cause }
