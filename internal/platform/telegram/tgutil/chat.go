package tgutil

import (
	"fmt"
	"strconv"

	"github.com/mymmrac/telego"
)

// ExtractChatID extracts a string chat identifier from a Telegram update.
// For forum topics, it returns "chatID/threadID".
func ExtractChatID(update telego.Update) string {
	var message *telego.Message
	if update.Message != nil {
		message = update.Message
	} else if update.EditedMessage != nil {
		message = update.EditedMessage
	} else if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
		m, ok := update.CallbackQuery.Message.(*telego.Message)
		if ok {
			message = m
		}
	}

	if message == nil {
		return "unknown"
	}

	chatID := message.Chat.ID
	if message.Chat.IsForum && message.MessageThreadID != 0 {
		return fmt.Sprintf("%d/%d", chatID, message.MessageThreadID)
	}
	return strconv.FormatInt(chatID, 10)
}
