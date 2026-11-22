package m0

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

const usage = `deskrealm-m0

  init --role center --dir DIR --center-host HOST --relay-host HOST
  init --role relay --dir DIR --center-key KEY
  config --dir DIR
  up --dir DIR
  status --dir DIR
  recreate --dir DIR
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	return runCLI(ctx, docker, args, stdout, stderr)
}

func runCLI(ctx context.Context, run runner, args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h" || args[0] == "help") {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	if len(args) == 0 {
		io.WriteString(stderr, usage)
		return errors.New("command is required")
	}
	command := args[0]
	switch command {
	case "init", "config", "up", "status", "recreate":
	default:
		return errors.New("unknown command; see --help")
	}

	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	dirArg := flags.String("dir", "", "dedicated deployment directory")
	var role, centerHost, relayHost, key string
	if command == "init" {
		flags.StringVar(&role, "role", "", "center or relay")
		flags.StringVar(&centerHost, "center-host", "", "center DNS name or IP")
		flags.StringVar(&relayHost, "relay-host", "", "advertised relay DNS name or IP")
		flags.StringVar(&key, "center-key", "", "center public key")
	}
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid flags; see --help")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	dir, err := deploymentDir(*dirArg)
	if err != nil {
		return err
	}
	var desired deployment
	if command == "init" {
		desired, err = desiredDeployment(dir, role, centerHost, relayHost, key)
		if err != nil {
			return err
		}
	}
	lock, err := lockDeployment(dir)
	if err != nil {
		return err
	}
	defer lock.Close()

	out := json.NewEncoder(stdout)
	out.SetIndent("", "  ")
	if command == "init" {
		d, err := initialize(desired)
		if err != nil {
			return err
		}
		return out.Encode(d)
	}
	d, err := loadDeployment(dir)
	if err != nil {
		return err
	}
	if command == "config" {
		body, err := d.compose(ctx, run, "config", "--format", "json")
		if err != nil {
			return err
		}
		_, err = stdout.Write(body)
		return err
	}
	state, err := d.lifecycle(ctx, run, command)
	if err != nil {
		return fmt.Errorf("%s %s: %w", command, d.Role, err)
	}
	return out.Encode(state)
}
