package worker

import (
	"context"
	"fmt"
	"strconv"

	"picoclaws/internal/platform/telegram/tgutil"
	"github.com/sipeed/picoclaw/pkg/tools"
)

type ReactionTool struct {
	token string
}

func NewReactionTool(token string) *ReactionTool {
	return &ReactionTool{token: token}
}

func (t *ReactionTool) Name() string {
	return "reaction"
}

func (t *ReactionTool) Description() string {
	return "Set an emoji reaction on a message. Example: reaction(emoji='👍')"
}

func (t *ReactionTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"emoji": map[string]any{
				"type":        "string",
				"description": "The emoji to use as a reaction (e.g., 👍, ❤️, 🔥)",
			},
			"message_id": map[string]any{
				"type":        "string",
				"description": "Optional: target message ID. Defaults to the current inbound message.",
			},
		},
		"required": []string{"emoji"},
	}
}

func (t *ReactionTool) Execute(ctx context.Context, args map[string]any) *tools.ToolResult {
	emoji, _ := args["emoji"].(string)
	msgIDStr, _ := args["message_id"].(string)

	if emoji == "" {
		return &tools.ToolResult{ForLLM: "emoji is required", IsError: true}
	}

	chatIDStr := tools.ToolChatID(ctx)
	if msgIDStr == "" {
		msgIDStr = tools.ToolMessageID(ctx)
	}

	if chatIDStr == "" || msgIDStr == "" {
		return &tools.ToolResult{ForLLM: "context missing chat_id or message_id", IsError: true}
	}

	mid, _ := strconv.Atoi(msgIDStr)
	
	if err := tgutil.SetReaction(ctx, t.token, chatIDStr, mid, emoji); err != nil {
		return &tools.ToolResult{
			ForLLM:  fmt.Sprintf("failed to set reaction: %v", err),
			IsError: true,
		}
	}

	return &tools.ToolResult{
		ForLLM: fmt.Sprintf("Reaction %s set on message %s", emoji, msgIDStr),
		Silent: true,
	}
}
