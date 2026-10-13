package provider

import (
	"errors"
	"net/http"
	"strings"

	"github.com/auucoder/gptgrok2api-go/internal/protocol"
)

// Only the active terminal assistant message is evidence of a text-only result.
// User prompts, analysis, historical branches and in-progress text are not.
func openAIImageTextError(value any) error {
	root, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	mapping, _ := root["mapping"].(map[string]any)
	node, _ := mapping[stringValue(root["current_node"])].(map[string]any)
	message, _ := node["message"].(map[string]any)
	author, _ := message["author"].(map[string]any)
	content, _ := message["content"].(map[string]any)
	if stringValue(author["role"]) != "assistant" || stringValue(content["content_type"]) != "text" || stringValue(message["channel"]) == "analysis" {
		return nil
	}
	ended, _ := message["end_turn"].(bool)
	if !ended && stringValue(message["status"]) != "finished_successfully" {
		return nil
	}
	parts, _ := content["parts"].([]any)
	texts := []string{}
	for _, part := range parts {
		if text, ok := part.(string); ok {
			texts = append(texts, text)
		}
	}
	text := strings.TrimSpace(strings.Join(texts, "\n"))
	if text == "" {
		return nil
	}
	if text == "由于我这边发生了错误，我未能生成图片。" || text == "抱歉，我无法生成这张图片，因为图像生成过程中发生了错误。" {
		return errors.Join(ErrImageGenerationStopped, &protocol.UpstreamError{Status: http.StatusBadGateway, Message: "Upstream image generation failed."})
	}
	return errors.Join(ErrImageTextReply, &protocol.UpstreamError{Status: http.StatusUnprocessableEntity, Message: "Upstream returned a text explanation without generating an image."})
}
