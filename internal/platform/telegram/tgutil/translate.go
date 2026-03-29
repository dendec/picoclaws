package tgutil

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mymmrac/telego"
	"github.com/sipeed/picoclaw/pkg/bus"
	"github.com/sipeed/picoclaw/pkg/channels"
	"github.com/sipeed/picoclaw/pkg/identity"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/utils"
)

// TranslateUpdate converts a telego.Update into a bus.InboundMessage.
// It replicates the logic from the official picoclaw telegram channel but adapted for single-shot execution.
func TranslateUpdate(ctx context.Context, bot *telego.Bot, update telego.Update, mediaStore media.MediaStore, botUsername string) (*bus.InboundMessage, error) {
	message := extractMessage(update)
	if message == nil {
		return nil, fmt.Errorf("no message in update")
	}

	user := message.From
	if user == nil {
		return nil, fmt.Errorf("message sender (user) is nil")
	}

	platformID := fmt.Sprintf("%d", user.ID)
	sender := bus.SenderInfo{
		Platform:    "telegram",
		PlatformID:  platformID,
		CanonicalID: identity.BuildCanonicalID("telegram", platformID),
		Username:    user.Username,
		DisplayName: user.FirstName,
	}

	chatID := message.Chat.ID
	chatIDStr := fmt.Sprintf("%d", chatID)
	messageIDStr := fmt.Sprintf("%d", message.MessageID)
	scope := channels.BuildMediaScope("telegram", chatIDStr, messageIDStr)

	content := ""
	mediaPaths := []string{}

	// Helper to register a local file with the media store
	storeMedia := func(localPath, filename string) string {
		if mediaStore != nil {
			ref, err := mediaStore.Store(localPath, media.MediaMeta{
				Filename: filename,
				Source:   "telegram",
			}, scope)
			if err == nil {
				return ref
			}
		}
		return localPath
	}

	if message.Text != "" {
		content += message.Text
	}
	if message.Caption != "" {
		if content != "" {
			content += "\n"
		}
		content += message.Caption
	}

	// Handle Media
	if len(message.Photo) > 0 {
		photo := message.Photo[len(message.Photo)-1]
		photoPath, _ := downloadPhoto(ctx, bot, photo.FileID)
		if photoPath != "" {
			mediaPaths = append(mediaPaths, storeMedia(photoPath, "photo.jpg"))
			if content != "" {
				content += "\n"
			}
			content += "[image: photo]"
		}
	}

	if message.Voice != nil {
		voicePath, _ := downloadFile(ctx, bot, message.Voice.FileID, ".ogg")
		if voicePath != "" {
			mediaPaths = append(mediaPaths, storeMedia(voicePath, "voice.ogg"))
			if content != "" {
				content += "\n"
			}
			content += "[voice]"
		}
	}

	if message.Audio != nil {
		audioPath, _ := downloadFile(ctx, bot, message.Audio.FileID, ".mp3")
		if audioPath != "" {
			mediaPaths = append(mediaPaths, storeMedia(audioPath, "audio.mp3"))
			if content != "" {
				content += "\n"
			}
			content += "[audio]"
		}
	}

	if message.Document != nil {
		docPath, _ := downloadFile(ctx, bot, message.Document.FileID, "")
		if docPath != "" {
			mediaPaths = append(mediaPaths, storeMedia(docPath, message.Document.FileName))
			if content != "" {
				content += "\n"
			}
			content += "[file]"
		}
	}

	if content == "" {
		content = "[empty message]"
	}

	// Forum topic handling
	compositeChatID := chatIDStr
	threadID := message.MessageThreadID
	if message.Chat.IsForum && threadID != 0 {
		compositeChatID = fmt.Sprintf("%d/%d", chatID, threadID)
	}

	// Mention stripping for groups
	if message.Chat.Type != "private" {
		if isBotMentioned(message, botUsername) {
			content = stripBotMention(content, botUsername)
		}
	}

	peerKind := "direct"
	peerID := fmt.Sprintf("%d", user.ID)
	if message.Chat.Type != "private" {
		peerKind = "group"
		peerID = compositeChatID
	}

	metadata := map[string]string{
		"user_id":       fmt.Sprintf("%d", user.ID),
		"username":      user.Username,
		"first_name":    user.FirstName,
		"language_code": user.LanguageCode,
		"is_group":      fmt.Sprintf("%t", message.Chat.Type != "private"),
	}
	if message.Chat.IsForum && threadID != 0 {
		metadata["parent_peer_kind"] = "topic"
		metadata["parent_peer_id"] = fmt.Sprintf("%d", threadID)
	}

	return &bus.InboundMessage{
		Channel:    "telegram",
		SenderID:   sender.CanonicalID,
		Sender:     sender,
		ChatID:     compositeChatID,
		Content:    content,
		Media:      mediaPaths,
		Peer:       bus.Peer{Kind: peerKind, ID: peerID},
		MessageID:  messageIDStr,
		MediaScope: scope,
		Metadata:   metadata,
	}, nil
}

func extractMessage(update telego.Update) *telego.Message {
	if update.Message != nil {
		return update.Message
	}
	if update.EditedMessage != nil {
		return update.EditedMessage
	}
	if update.CallbackQuery != nil && update.CallbackQuery.Message != nil {
		if m, ok := update.CallbackQuery.Message.(*telego.Message); ok {
			return m
		}
	}
	return nil
}

func downloadPhoto(ctx context.Context, bot *telego.Bot, fileID string) (string, error) {
	file, err := bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return "", err
	}
	return downloadFileWithInfo(bot, file, ".jpg"), nil
}

func downloadFile(ctx context.Context, bot *telego.Bot, fileID, ext string) (string, error) {
	file, err := bot.GetFile(ctx, &telego.GetFileParams{FileID: fileID})
	if err != nil {
		return "", err
	}
	return downloadFileWithInfo(bot, file, ext), nil
}

func downloadFileWithInfo(bot *telego.Bot, file *telego.File, ext string) string {
	if file.FilePath == "" {
		return ""
	}
	url := bot.FileDownloadURL(file.FilePath)
	filename := file.FilePath + ext
	return utils.DownloadFile(url, filename, utils.DownloadOptions{
		LoggerPrefix: "telegram",
	})
}

func isBotMentioned(message *telego.Message, botUsername string) bool {
	text, entities := telegramEntityTextAndList(message)
	if text == "" || len(entities) == 0 {
		return false
	}
	runes := []rune(text)
	for _, entity := range entities {
		entityText, ok := telegramEntityText(runes, entity)
		if !ok {
			continue
		}
		switch entity.Type {
		case telego.EntityTypeMention:
			if botUsername != "" && strings.EqualFold(entityText, "@"+botUsername) {
				return true
			}
		case telego.EntityTypeTextMention:
			if botUsername != "" && entity.User != nil && strings.EqualFold(entity.User.Username, botUsername) {
				return true
			}
		case telego.EntityTypeBotCommand:
			if isBotCommandEntityForThisBot(entityText, botUsername) {
				return true
			}
		}
	}
	return false
}

func telegramEntityTextAndList(message *telego.Message) (string, []telego.MessageEntity) {
	if message.Text != "" {
		return message.Text, message.Entities
	}
	return message.Caption, message.CaptionEntities
}

func telegramEntityText(runes []rune, entity telego.MessageEntity) (string, bool) {
	if entity.Offset < 0 || entity.Length <= 0 {
		return "", false
	}
	end := entity.Offset + entity.Length
	if entity.Offset >= len(runes) || end > len(runes) {
		return "", false
	}
	return string(runes[entity.Offset:end]), true
}

func isBotCommandEntityForThisBot(entityText, botUsername string) bool {
	if !strings.HasPrefix(entityText, "/") {
		return false
	}
	command := strings.TrimPrefix(entityText, "/")
	if command == "" {
		return false
	}
	at := strings.IndexRune(command, '@')
	if at == -1 {
		return true
	}
	mentionUsername := command[at+1:]
	return strings.EqualFold(mentionUsername, botUsername)
}

func stripBotMention(content string, botUsername string) string {
	if botUsername == "" {
		return content
	}
	re := regexp.MustCompile(`(?i)@` + regexp.QuoteMeta(botUsername))
	content = re.ReplaceAllString(content, "")
	return strings.TrimSpace(content)
}

func InferMediaType(filename, contentType string) string {
	ct := strings.ToLower(contentType)
	fn := strings.ToLower(filename)

	if strings.HasPrefix(ct, "image/") {
		return "image"
	}
	if strings.HasPrefix(ct, "audio/") || ct == "application/ogg" {
		return "audio"
	}
	if strings.HasPrefix(ct, "video/") {
		return "video"
	}

	ext := filepath.Ext(fn)
	switch ext {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp", ".svg":
		return "image"
	case ".mp3", ".wav", ".ogg", ".m4a", ".flac", ".aac", ".w_ma", ".opus":
		return "audio"
	case ".mp4", ".avi", ".mov", ".webm", ".mkv":
		return "video"
	}

	return "file"
}
