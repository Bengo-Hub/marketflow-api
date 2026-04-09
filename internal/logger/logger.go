package logger

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New initialises a zap logger tuned for the provided environment.
func New(env string) (*zap.Logger, error) {
	cfg := zap.NewProductionConfig()
	if env == "development" || env == "local" {
		cfg = zap.NewDevelopmentConfig()
		cfg.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	}
	cfg.Level = zap.NewAtomicLevelAt(zap.InfoLevel)
	return cfg.Build()
}

// ZapError is a helper to avoid importing zap in every package.
func ZapError(err error) zap.Field {
	return zap.Error(err)
}
