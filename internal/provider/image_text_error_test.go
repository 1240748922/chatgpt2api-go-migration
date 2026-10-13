package provider

import (
	"errors"
	"testing"
)

func TestImageTextErrorUsesOnlyActiveTerminalAssistant(t *testing.T) {
	message := map[string]any{"author": map[string]any{"role": "assistant"}, "status": "finished_successfully", "content": map[string]any{"content_type": "text", "parts": []any{"由于我这边发生了错误，我未能生成图片。"}}}
	root := map[string]any{"current_node": "active", "mapping": map[string]any{"active": map[string]any{"message": message}}}
	if !errors.Is(openAIImageTextError(root), ErrImageGenerationStopped) {
		t.Fatal("terminal generation error was missed")
	}
	message["content"] = map[string]any{"content_type": "text", "parts": []any{"Please revise the image prompt."}}
	if !errors.Is(openAIImageTextError(root), ErrImageTextReply) {
		t.Fatal("terminal text was missed")
	}
	message["channel"] = "analysis"
	if openAIImageTextError(root) != nil {
		t.Fatal("analysis text was treated as a result")
	}
	delete(message, "channel")
	message["status"] = "in_progress"
	if openAIImageTextError(root) != nil {
		t.Fatal("unfinished text was treated as a result")
	}
	message["status"] = "finished_successfully"
	message["author"] = map[string]any{"role": "user"}
	message["metadata"] = map[string]any{"status": "failed", "error": "quoted test input"}
	if openAIImageTextError(root) != nil || openAIImageTerminalError(root) != nil {
		t.Fatal("user data was classified as a provider failure")
	}
	root["current_node"] = "missing"
	message["author"] = map[string]any{"role": "assistant"}
	if openAIImageTextError(root) != nil {
		t.Fatal("historical branch was treated as the active reply")
	}
}
