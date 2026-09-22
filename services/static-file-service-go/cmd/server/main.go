// Command server is the Go port of services/static_file_service (Rust).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/sqs"

	"github.com/dungvv/next/services/static-file-service-go/internal/api"
	"github.com/dungvv/next/services/static-file-service-go/internal/authz"
	"github.com/dungvv/next/services/static-file-service-go/internal/config"
	metadatadb "github.com/dungvv/next/services/static-file-service-go/internal/dynamodb"
	"github.com/dungvv/next/services/static-file-service-go/internal/events"
	"github.com/dungvv/next/services/static-file-service-go/internal/s3client"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx := context.Background()

	cfg, err := config.Load(ctx)
	if err != nil {
		slog.Error("missing environment variables", "error", err)
		os.Exit(1)
	}

	awsCfg, err := config.LoadAWSConfig(ctx, cfg.LocalAWSURL)
	if err != nil {
		slog.Error("failed to load aws config", "error", err)
		os.Exit(1)
	}

	metadataClient := metadatadb.New(dynamodb.NewFromConfig(awsCfg), cfg.DynamoDBTableName)
	sqsClient := sqs.NewFromConfig(awsCfg)
	storageClient := s3client.New(awsCfg, cfg.StaticStorageBucket, cfg.LocalAWSURL, cfg.LocalAWSPublicURL)

	authorizer, err := authz.New(authz.Config{
		InternalAPIKey:         cfg.InternalAPIKey,
		DefaultInternalUserID:  authz.MacroInternalUserID,
		JWTSecret:              cfg.JWTSecretKey,
		Audience:               cfg.Audience,
		Issuer:                 cfg.Issuer,
		MacroAPITokenIssuer:    cfg.MacroAPITokenIssuer,
		MacroAPITokenPublicKey: cfg.MacroAPITokenPublicKey,
		Environment:            string(cfg.Environment),
	})
	if err != nil {
		slog.Error("failed to build authorizer", "error", err)
		os.Exit(1)
	}

	server := &api.Server{
		Config:    cfg,
		Metadata:  metadataClient,
		Storage:   storageClient,
		Authorize: authorizer,
	}

	if !cfg.DisableEventPoll {
		pollCtx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer cancel()
		go events.PollS3Events(pollCtx, sqsClient, cfg.S3EventQueue, metadataClient)
	}

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.Port)
	slog.Info("💀 static_file_service 💀", "environment", cfg.Environment, "port", cfg.Port)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil {
		slog.Error("error starting service", "error", err)
		os.Exit(1)
	}
}
