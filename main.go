// Command np shares plain-text notes between machines on a Tailscale tailnet.
package main

import (
	"os"

	"github.com/EmreErdogan/np/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
