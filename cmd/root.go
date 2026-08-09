package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/urfave/cli/v2"
	"go.uber.org/zap"

	"github.com/lambda-feedback/shimmy/config"
	"github.com/lambda-feedback/shimmy/util/conf"
	"github.com/lambda-feedback/shimmy/util/logging"
)

var (
	appName  = "shimmy"
	appUsage = `A shim for running arbitrary, language-agnostic evaluation
functions on arbitrary, serverless platforms.`
	rootApp = &cli.App{
		Name:            appName,
		Usage:           appUsage,
		HideHelpCommand: true,
		Args:            true,
		DefaultCommand:  "run",
		Flags: []cli.Flag{
			// general flags
			&cli.StringFlag{
				Name:    "log-level",
				Usage:   "set the log level. Options: debug, info, warn, error, panic, fatal.",
				EnvVars: []string{"LOG_LEVEL"},
			},
			&cli.StringFlag{
				Name:    "log-format",
				Usage:   "set the log format. Options: production, development.",
				EnvVars: []string{"LOG_FORMAT"},
			},
			// auth flags
			&cli.StringFlag{
				Name:     "auth-key",
				Usage:    "the secret key for the application.",
				Category: "auth",
				EnvVars:  []string{"AUTH_KEY"},
			},
			// shim flags
			&cli.StringFlag{
				Name:     "interface",
				Aliases:  []string{"i"},
				Usage:    "the execution interface. Options: rpc, file, wasm, pyodide.",
				Value:    "rpc",
				Category: "function",
				EnvVars:  []string{"FUNCTION_INTERFACE"},
			},
			&cli.StringFlag{
				Name:     "command",
				Aliases:  []string{"c"},
				Usage:    "the command to invoke to start the worker process.",
				Category: "function",
				EnvVars:  []string{"FUNCTION_COMMAND"},
				// Not required for wasm, which manages
				// the worker process internally via wazero.
			},
			&cli.StringFlag{
				Name:     "cwd",
				Aliases:  []string{"d"},
				Usage:    "the working directory for the worker process.",
				Category: "function",
				EnvVars:  []string{"FUNCTION_WORKING_DIR"},
			},
			&cli.StringSliceFlag{
				Name:     "arg",
				Aliases:  []string{"a"},
				Usage:    "additional arguments for to the worker process.",
				Category: "function",
				EnvVars:  []string{"FUNCTION_ARGS"},
			},
			&cli.StringSliceFlag{
				Name:     "env",
				Aliases:  []string{"e"},
				Usage:    "additional environment variables for the worker process.",
				Category: "function",
				EnvVars:  []string{"FUNCTION_ENV"},
			},
			&cli.IntFlag{
				Name:        "max-workers",
				Aliases:     []string{"n"},
				Usage:       "the maximum number of worker processes to run concurrently.",
				DefaultText: "number of CPU cores",
				Value:       0,
				Category:    "function",
				EnvVars:     []string{"FUNCTION_MAX_PROCS"},
			},
			&cli.DurationFlag{
				Name:     "worker-stop-timeout",
				Usage:    "the duration to wait for a worker process to stop.",
				Value:    5 * time.Second,
				Category: "worker",
				EnvVars:  []string{"FUNCTION_WORKER_STOP_TIMEOUT"},
			},
			&cli.DurationFlag{
				Name:     "worker-send-timeout",
				Usage:    "the timeout for a single message send operation.",
				Value:    30 * time.Second,
				Category: "worker",
				EnvVars:  []string{"FUNCTION_WORKER_SEND_TIMEOUT"},
			},
			&cli.StringFlag{
				Name:     "rpc-transport",
				Aliases:  []string{"t"},
				Usage:    "the transport to use for the RPC interface. Options: stdio, ipc, http, tcp, ws.",
				Value:    "stdio",
				EnvVars:  []string{"FUNCTION_RPC_TRANSPORT"},
				Category: "rpc",
			},
			&cli.StringFlag{
				Name:     "rpc-transport-ipc-endpoint",
				Usage:    "the IPC endpoint to use for the IPC transport. Default: /tmp/eval.sock",
				EnvVars:  []string{"FUNCTION_RPC_TRANSPORT_IPC_ENDPOINT"},
				Category: "rpc",
			},
			&cli.StringFlag{
				Name:     "rpc-transport-http-url",
				Usage:    "the url to use for the HTTP transport. Default: http://127.0.0.1:7321",
				EnvVars:  []string{"FUNCTION_RPC_TRANSPORT_HTTP_URL"},
				Value:    "http://127.0.0.1:7321",
				Category: "rpc",
			},
			&cli.StringFlag{
				Name:     "rpc-transport-ws-url",
				Usage:    "the url to use for the WebSocket transport. Default: ws://127.0.0.1:7321",
				EnvVars:  []string{"FUNCTION_RPC_TRANSPORT_WS_URL"},
				Value:    "ws://127.0.0.1:7321",
				Category: "rpc",
			},
			&cli.StringFlag{
				Name:     "rpc-transport-tcp-address",
				Usage:    "the address to use for the TCP transport. Default: 127.0.0.1:7321",
				EnvVars:  []string{"FUNCTION_RPC_TRANSPORT_TCP_ADDRESS"},
				Value:    "127.0.0.1:7321",
				Category: "rpc",
			},
		},
		Before: func(ctx *cli.Context) error {
			// create the logger
			log, err := createLogger(ctx)
			if err != nil {
				return err
			}

			// inject logger into cli context
			ctx.Context = logging.ContextWithLogger(ctx.Context, log)

			// create the config
			cfg, err := parseRootConfig(ctx)
			if err != nil {
				return err
			}

			// inject the config into the cli context
			ctx.Context = conf.ContextWithConfig(ctx.Context, cfg)

			return nil
		},
		After: func(ctx *cli.Context) error {
			// flush the logger
			if log, err := logging.LoggerFromContext(ctx.Context); err == nil {
				log.Sync()
			}

			return nil
		},
	}
)

func init() {
	cli.VersionFlag = &cli.BoolFlag{
		Name:               "version",
		Usage:              "print the version",
		DisableDefaultText: true,
	}
}

type ExecuteParams struct {
	Version  string
	Compiled time.Time
}

func Execute(params ExecuteParams) {
	rootApp.Version = params.Version
	rootApp.Compiled = params.Compiled

	run(context.Background(), os.Args)
}

func run(ctx context.Context, args []string) {
	err := rootApp.RunContext(ctx, args)

	// if app exited without error, return
	if err == nil {
		return
	}

	fmt.Printf("exit error: %s\n", err.Error())

	// if app exited with ExitError, exit with given exit code

	// otherwise, exit with exit code 1
	os.Exit(1)
}

func createLogger(ctx *cli.Context) (*zap.Logger, error) {
	level := getLogLevelFromCLI(ctx)
	format := getLogFormatFromCLI(ctx)

	var config zap.Config
	switch format {
	case "production":
		config = zap.NewProductionConfig()
	case "development":
		config = zap.NewDevelopmentConfig()
	default:
		return nil, fmt.Errorf("invalid log format: %s", format)
	}

	config.InitialFields = map[string]any{}

	config.Level = level

	return config.Build()
}

func getLogFormatFromCLI(ctx *cli.Context) string {
	if format := ctx.String("log-format"); format != "" {
		return format
	}

	return "development"
}

func getLogLevelFromCLI(ctx *cli.Context) zap.AtomicLevel {
	if atom, err := zap.ParseAtomicLevel(ctx.String("log-level")); err == nil {
		return atom
	}

	return zap.NewAtomicLevelAt(zap.InfoLevel)
}

func parseRootConfig(ctx *cli.Context) (config.Config, error) {

	// map cli flags to config fields
	cliMap := map[string]string{
		"auth-key":                   "auth.key",
		"max-workers":                "runtime.max_workers",
		"command":                    "runtime.cmd",
		"cwd":                        "runtime.cwd",
		"arg":                        "runtime.arg",
		"env":                        "runtime.env",
		"interface":                  "runtime.io.interface",
		"rpc-transport":              "runtime.io.rpc.transport",
		"rpc-transport-ipc-endpoint": "runtime.io.rpc.ipc.endpoint",
		"rpc-transport-http-url":     "runtime.io.rpc.http.url",
		"rpc-transport-ws-url":       "runtime.io.rpc.ws.url",
		"rpc-transport-tcp-address":  "runtime.io.rpc.tcp.address",
		"worker-send-timeout":        "runtime.send.timeout",
		"worker-stop-timeout":        "runtime.stop.timeout",
	}

	// parse config using env
	cfg, err := conf.Parse[config.Config](conf.ParseOptions{
		Cli:    ctx,
		CliMap: cliMap,
	})
	if err != nil {
		return config.Config{}, err
	}

	return cfg, err
}
