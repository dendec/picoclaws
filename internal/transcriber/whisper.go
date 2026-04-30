package transcriber

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/sipeed/picoclaw/pkg/audio/asr"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/media"
	"github.com/sipeed/picoclaw/pkg/providers"
)

type WhisperTranscriber struct {
	client     *openai.Client
	modelID    string
	mediaStore media.MediaStore
}

func NewTranscriber(cfg *config.Config, store media.MediaStore) (*WhisperTranscriber, error) {
	modelName := cfg.Voice.ModelName
	if modelName == "" {
		return nil, fmt.Errorf("voice model_name not configured in config.voice")
	}

	modelCfg, err := cfg.GetModelConfig(modelName)
	if err != nil {
		return nil, fmt.Errorf("voice model %q not found in model_list: %w", modelName, err)
	}

	apiKey := modelCfg.APIKey()
	if apiKey == "" {
		return nil, fmt.Errorf("api key not found for voice model %q", modelName)
	}

	// Resolve API base, default to DeepInfra if protocol matches
	apiBase := modelCfg.APIBase
	if apiBase == "" {
		if strings.HasPrefix(modelCfg.Model, "deepinfra/") {
			apiBase = "https://api.deepinfra.com/v1/openai"
		} else {
			apiBase = "https://api.openai.com/v1"
		}
	}

	client := openai.NewClient(
		option.WithAPIKey(apiKey),
		option.WithBaseURL(apiBase),
	)

	// Extract actual model ID (strip protocol)
	_, modelID := providers.ExtractProtocol(modelCfg)
	if modelID == "" {
		modelID = modelCfg.Model
	}

	return &WhisperTranscriber{
		client:     &client,
		modelID:    modelID,
		mediaStore: store,
	}, nil
}

func (t *WhisperTranscriber) Name() string {
	return "whisper-transcriber"
}

func (t *WhisperTranscriber) Transcribe(ctx context.Context, path string) (*asr.TranscriptionResponse, error) {
	// 1. Transcode to mp3 using ffmpeg
	tmpMP3 := path + ".mp3"
	defer os.Remove(tmpMP3)

	cmd := exec.CommandContext(ctx, "ffmpeg", "-y", "-i", path, "-ar", "16000", "-ac", "1", "-b:a", "64k", tmpMP3)
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("ffmpeg conversion failed: %w (output: %s)", err, string(out))
	}

	// 2. Open the file for the SDK
	f, err := os.Open(tmpMP3)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 3. Transcribe using openai-go SDK
	res, err := t.client.Audio.Transcriptions.New(ctx, openai.AudioTranscriptionNewParams{
		File:  f,
		Model: openai.AudioModel(t.modelID),
	})
	if err != nil {
		return nil, fmt.Errorf("asr api error: %w", err)
	}

	return &asr.TranscriptionResponse{
		Text: strings.TrimSpace(res.Text),
	}, nil
}
