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
// It replicates the core logic from the official picoclaw telegram channel,
// adapted for single-shot serverless execution.
func TranslateUpdate(ctx context.Context, bot *telego.Bot, update telego.Update, mediaStore media.MediaStore, botUsername string) (*bus.InboundMessage, error) {
	message := ExtractMessage(update)
	if message == nil {
		return nil, fmt.Errorf("no message found in update: %v", update.UpdateID)
	}

	user := message.From
	if user == nil {
		return nil, fmt.Errorf("message sender is missing")
	}

	sender := buildSenderInfo(user)
	chatIDStr := fmt.Sprintf("%d", message.Chat.ID)
	messageIDStr := fmt.Sprintf("%d", message.MessageID)
	scope := channels.BuildMediaScope("telegram", chatIDStr, messageIDStr)

	// Combine Text and Caption
	content := joinTextAndCaption(message.Text, message.Caption)

	// Process all media types
	mediaPaths, mediaDesc := processMedia(ctx, bot, message, mediaStore, scope)
	if mediaDesc != "" {
		if content != "" {
			content += "\n"
		}
		content += mediaDesc
	}

	if content == "" {
		content = "[empty message]"
	}

	// Calculate composite IDs for forums and groups
	compositeChatID := buildCompositeChatID(message)
	peerKind, _ := buildPeerInfo(message, compositeChatID)

	metadata := buildMetadata(user, message, threadID(message))

	// Stripping bot mentions in groups
	if message.Chat.Type != "private" && isBotMentioned(message, botUsername) {
		content = stripBotMention(content, botUsername)
	}

	inMsg := &bus.InboundMessage{
		Context: bus.InboundContext{
			Channel:   "telegram",
			ChatID:    compositeChatID,
			ChatType:  peerKind,
			SenderID:  sender.CanonicalID,
			MessageID: messageIDStr,
			Raw:       metadata,
		},
		Sender:     sender,
		Content:    content,
		Media:      mediaPaths,
		SessionKey: compositeChatID,
		Channel:    "telegram",
		SenderID:   sender.CanonicalID,
		ChatID:     compositeChatID,
		MessageID:  messageIDStr,
		MediaScope: scope,
	}

	if message.Chat.IsForum && message.MessageThreadID != 0 {
		inMsg.Context.TopicID = fmt.Sprintf("%d", message.MessageThreadID)
	}

	return inMsg, nil
}

// Internal helper functions to keep TranslateUpdate clean

func buildSenderInfo(user *telego.User) bus.SenderInfo {
	platformID := fmt.Sprintf("%d", user.ID)
	return bus.SenderInfo{
		Platform:    "telegram",
		PlatformID:  platformID,
		CanonicalID: identity.BuildCanonicalID("telegram", platformID),
		Username:    user.Username,
		DisplayName: user.FirstName,
	}
}

func joinTextAndCaption(text, caption string) string {
	if text == "" {
		return caption
	}
	if caption == "" {
		return text
	}
	return text + "\n" + caption
}

func buildCompositeChatID(m *telego.Message) string {
	id := fmt.Sprintf("%d", m.Chat.ID)
	if m.Chat.IsForum && m.MessageThreadID != 0 {
		return fmt.Sprintf("%d/%d", m.Chat.ID, m.MessageThreadID)
	}
	return id
}

func buildPeerInfo(m *telego.Message, compositeID string) (string, string) {
	if m.Chat.Type == "private" {
		return "direct", fmt.Sprintf("%d", m.From.ID)
	}
	return "group", compositeID
}

func threadID(m *telego.Message) int {
	return m.MessageThreadID
}

func buildMetadata(user *telego.User, m *telego.Message, threadID int) map[string]string {
	meta := map[string]string{
		"user_id":       fmt.Sprintf("%d", user.ID),
		"username":      user.Username,
		"first_name":    user.FirstName,
		"language_code": user.LanguageCode,
		"is_group":      fmt.Sprintf("%t", m.Chat.Type != "private"),
	}
	if m.Chat.IsForum && threadID != 0 {
		meta["parent_peer_kind"] = "topic"
		meta["parent_peer_id"] = fmt.Sprintf("%d", threadID)
	}
	return meta
}

func processMedia(ctx context.Context, bot *telego.Bot, m *telego.Message, store media.MediaStore, scope string) ([]string, string) {
	var paths []string
	var descParts []string

	// Local helper to store and append
	add := func(localPath, filename, desc string) {
		if localPath == "" {
			return
		}
		finalPath := localPath
		if store != nil {
			if ref, err := store.Store(localPath, media.MediaMeta{Filename: filename, Source: "telegram"}, scope); err == nil {
				finalPath = ref
			}
		}
		paths = append(paths, finalPath)
		descParts = append(descParts, desc)
	}

	// 1. Photos
	if len(m.Photo) > 0 {
		p := m.Photo[len(m.Photo)-1]
		local, _ := downloadPhoto(ctx, bot, p.FileID)
		add(local, "photo.jpg", "[image: photo]")
	}

	// 2. Voice
	if m.Voice != nil {
		local, _ := downloadFile(ctx, bot, m.Voice.FileID, ".ogg")
		add(local, "voice.ogg", "[voice]")
	}

	// 3. Audio
	if m.Audio != nil {
		local, _ := downloadFile(ctx, bot, m.Audio.FileID, ".mp3")
		add(local, "audio.mp3", "[audio]")
	}

	// 4. Documents
	if m.Document != nil {
		local, _ := downloadFile(ctx, bot, m.Document.FileID, "")
		add(local, m.Document.FileName, "[file]")
	}

	return paths, strings.Join(descParts, "\n")
}

// ExtractMessage retrieves the message object from different update types (message, edited, callback).
func ExtractMessage(update telego.Update) *telego.Message {
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
