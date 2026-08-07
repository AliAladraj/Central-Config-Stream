package configclient_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/AliAladraj/Central-Config-Stream/pkg/configclient"
)

// ExampleNew shows the path a consuming service takes: warm the cache once at
// boot, then serve every read from memory for the life of the process.
func ExampleNew() {
	// A throwaway control plane so this example runs; a real deployment has
	// central-config publishing to its own NATS cluster.
	natsURL, stop := exampleControlPlane()
	defer stop()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client, err := configclient.New(ctx, configclient.Options{
		NATSURL:        natsURL, // e.g. nats://nats:4222
		EnvironmentID:  3,
		MicroserviceID: 1,
	})
	if err != nil {
		log.Fatalf("configclient: %v", err)
	}
	defer client.Close()

	fmt.Println("search_v2:", client.FlagEnabled("search_v2"))
	if settings, ok := client.MicroSettings(1); ok {
		fmt.Println("appsettings:", string(settings))
	}
	if title, ok := client.Translate(1, "pt-BR", "catalog.title"); ok {
		fmt.Println("catalog.title:", title)
	}

	// Output:
	// search_v2: true
	// appsettings: {"timeout":30}
	// catalog.title: Catálogo
}
