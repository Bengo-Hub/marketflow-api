package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bengobox/marketflow-api/internal/app"
	"github.com/bengobox/marketflow-api/internal/config"
	"github.com/bengobox/marketflow-api/internal/logger"
	"github.com/joho/godotenv"
)

// @title MarketFlow API
// @version 1.0.0
// @description AI-powered marketing automation platform API. Manages leads, contacts, campaigns, funnels, knowledge base, and AI chat sessions for FLP FBOs and generic businesses.
// @BasePath /api/v1
// @schemes https http
// @host marketflowapi.codevertexafrica.com
// @securityDefinitions.apikey bearerAuth
// @in header
// @name Authorization
// @description JWT token from auth-service (SSO). Format: Bearer {token}
func main() {
	if err := godotenv.Load(); err != nil {
		log.Printf("warning: could not load .env file: %v", err)
	} else {
		log.Println("ok: .env file loaded successfully")
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	zapLogger, err := logger.New(cfg.App.Environment)
	if err != nil {
		log.Fatalf("failed to init logger: %v", err)
	}
	defer zapLogger.Sync() //nolint:errcheck // best effort

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	application, err := app.New(ctx, cfg, zapLogger)
	if err != nil {
		zapLogger.Fatal("failed to bootstrap application", logger.ZapError(err))
	}

	go func() {
		if err := application.Run(); err != nil {
			zapLogger.Fatal("server encountered error", logger.ZapError(err))
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	zapLogger.Info("shutdown signal received")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer shutdownCancel()

	if err := application.Shutdown(shutdownCtx); err != nil {
		zapLogger.Error("graceful shutdown failed", logger.ZapError(err))
		time.Sleep(2 * time.Second)
	}
}
