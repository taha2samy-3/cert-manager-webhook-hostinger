package main

import (
	"fmt"
	"os"
	"strings"

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

	solver := hostinger.New("cert-manager-webhook-hostinger/" + version)
	// Comma-separated API endpoints that issuers may set as serverURL,
	// besides the default one. Set by the chart value allowedServerURLs.
	if urls := os.Getenv("HOSTINGER_ALLOWED_SERVER_URLS"); urls != "" {
		solver.AllowServerURLs(strings.Split(urls, ",")...)
	}

	cmd.RunWebhookServer(groupName, solver)
}
