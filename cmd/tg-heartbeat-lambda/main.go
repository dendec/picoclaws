package main

import (
	"context"
	"os"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"picoclaws/internal/heartbeat"
)

func main() {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	if strings.ToLower(os.Getenv("LOG_LEVEL")) == "debug" {
		zerolog.SetGlobalLevel(zerolog.DebugLevel)
	} else {
		zerolog.SetGlobalLevel(zerolog.InfoLevel)
	}
	zerolog.DefaultContextLogger = &log.Logger

	ctx := context.Background()
	app, err := heartbeat.NewHeartbeatApp(ctx)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize HeartbeatApp")
	}

	log.Info().Msg("Starting Heartbeat Dispatcher Lambda...")
	lambda.Start(app.Handle)
}
