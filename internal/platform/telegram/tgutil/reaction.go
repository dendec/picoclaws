package tgutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// AllowedEmojis is the precise list of standard Telegram reactions.
var AllowedEmojis = []string{
	"❤", "👍", "👎", "🔥", "🥰", "👏", "😁", "🤔", "🤯", "😱", "🤬", "😢", "🎉", "🤩", "🤮", "💩", "🙏", "👌", "🕊", "🤡", "🥱", "🥴", "😍", "🐳", "❤🔥", "🌚", "🌭", "💯", "🤣", "⚡", "🍌", "🏆", "💔", "🤨", "😐", "🍓", "🍾", "💋", "🖕", "😈", "😴", "😭", "🤓", "👻", "👨💻", "👀", "🎃", "🙈", "😇", "😨", "🤝", "✍", "🤗", "🫡", "🎅", "🎄", "☃", "💅", "🤪", "🗿", "🆒", "💘", "🙉", "🦄", "😘", "💊", "🙊", "😎", "👾", "🤷♂", "🤷", "🤷♀", "😡",
}

type tgReaction struct {
	Type  string `json:"type"`
	Emoji string `json:"emoji"`
}

type reactionReq struct {
	ChatID    string       `json:"chat_id"`
	MessageID int          `json:"message_id"`
	Reaction  []tgReaction `json:"reaction"`
}

// SetReaction calls Telegram API to set a reaction.
func SetReaction(ctx context.Context, token, chatID string, messageID int, emoji string) error {
	payload := reactionReq{
		ChatID:    chatID,
		MessageID: messageID,
		Reaction: []tgReaction{
			{Type: "emoji", Emoji: emoji},
		},
	}

	body, _ := json.Marshal(payload)
	url := fmt.Sprintf("https://api.telegram.org/bot%s/setMessageReaction", token)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(body))
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("http execute: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("API error (%d): use one of allowed emojis: %v", resp.StatusCode, AllowedEmojis)
	}

	return nil
}
