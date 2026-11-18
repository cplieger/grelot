// Command grelot is the grelot server, which coordinates coding agents
// through one HTTP API, its healthcheck, and the client every agent runs.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/cplieger/grelot/internal/cli"
	"github.com/cplieger/grelot/internal/client"
	"github.com/cplieger/grelot/internal/proctree"
	"github.com/cplieger/grelot/internal/provider"
	buildver "github.com/cplieger/grelot/internal/version"
	"github.com/cplieger/grelot/internal/where"
	"github.com/cplieger/grelot/providers"
)

// version is set at build time with -ldflags "-X main.version=<tag>".
var version = "dev"

// buildVersion is the version this binary reports; buildver.Build says
// which.
func buildVersion() string { return buildver.Build(version, debug.ReadBuildInfo) }

// The server's commands; serve is also what grelot runs with no arguments.
// The last three reach the running server through its admin socket.
const (
	cmdServe      = "serve"
	cmdHealth     = "health"
	cmdSetupToken = "setup-token"
	cmdRecovery   = "recovery"
	cmdKey        = "key"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches one subcommand and returns the process exit code: 0 on
// success, 1 on a runtime failure, 2 on a usage error.
func run(args []string, stdout, stderr io.Writer) int {
	cmd, rest := cmdServe, args
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}
	switch cmd {
	case cmdServe, cmdHealth, cmdSetupToken, cmdRecovery, cmdKey:
		return serverCommand(cmd, rest, stdout, stderr)
	case "provider":
		return providerCommand(args[1:], stderr)
	default:
		return clientCommand(args, stdout, stderr)
	}
}

// clientCommand runs one of the client's verbs in this process's
// environment.
func clientCommand(args []string, stdout, stderr io.Writer) int {
	configDir, err := os.UserConfigDir()
	if err != nil {
		fmt.Fprintf(stderr, "grelot cannot find the user config dir: %v; set HOME, or XDG_CONFIG_HOME on Linux\n", err)
		return 1
	}
	set, err := provider.LoadSet(providers.FS, os.DirFS(filepath.Join(client.Dir(configDir), "providers")))
	if err != nil {
		fmt.Fprintf(stderr, "grelot: %v\n", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	reported := buildVersion()
	return cli.Run(ctx, &cli.Env{
		Stdin: os.Stdin, Stdout: stdout, Stderr: stderr,
		Lookup: os.LookupEnv, Hostname: os.Hostname, Getwd: os.Getwd, Home: os.UserHomeDir,
		Machine: where.Machine, InContainer: where.InContainer,
		Providers: set, Processes: proctree.System(),
		ConfigDir: configDir, Version: buildver.Wire(reported), Build: reported, PID: os.Getpid(),
	}, args)
}
