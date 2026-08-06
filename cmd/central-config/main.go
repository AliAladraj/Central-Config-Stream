// Command central-config is the control-plane service: an HTTP admin API over
// the configuration database that also writes every change through to NATS
// JetStream KV, where consuming microservices watch and cache it.
//
// This file is only the entry point. It loads .env, brings up logging, and
// hands off to internal/app, which does the assembly and owns the server
// lifecycle. Both failure paths exit non-zero after saying why in the same
// structured shape as the rest of the service's output.
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
