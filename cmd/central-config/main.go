package main

import (
	"os"

	"github.com/ErasedKyte/Central-Config-Stream/internal/app"
	"github.com/ErasedKyte/Central-Config-Stream/internal/obs"

	"github.com/joho/godotenv"
)

func main() {
	// .env is loaded before the logger is configured — it is where LOG_LEVEL
	// and LOG_FORMAT come from — so its outcome is reported once logging is up.
	envErr := godotenv.Load()

	cfg := app.LoadConfig()
	obs.Setup(cfg.LogOptions())

	logger := obs.Logger("startup")
	if envErr != nil {
		logger.Info("no .env file found, using system environment variables")
	}

	application, err := app.NewApp(cfg)
	if err != nil {
		// These were log.Fatalf: the process still stops, it just says why in
		// the same structured shape as everything else first.
		logger.Error("failed to initialize app", obs.Err(err))
		os.Exit(1)
	}

	// Run logs the listening address itself, once it knows whether it is TLS.
	if err := application.Run(); err != nil {
		logger.Error("failed to run app", obs.Err(err))
		os.Exit(1)
	}
}
