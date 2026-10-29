package main

import (
	"fmt"
	"os"

	"github.com/cert-manager/cert-manager/pkg/acme/webhook/cmd"

	"github.com/taha2samy-3/cert-manager-webhook-hostinger/internal/hostinger"
)

// version is set at build time via -ldflags.
var version = "dev"

func main() {
	groupName := os.Getenv("GROUP_NAME")
	if groupName == "" {
		fmt.Fprintln(os.Stderr, "GROUP_NAME must be specified")
		os.Exit(1)
	}

	cmd.RunWebhookServer(groupName,
		hostinger.New("cert-manager-webhook-hostinger/"+version),
	)
}
