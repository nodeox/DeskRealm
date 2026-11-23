package install

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const usage = `DeskRealm center installer

  deskrealm-install preflight --config FILE
  deskrealm-install prepare   --config FILE
  deskrealm-install install   --config FILE
  deskrealm-install status    --config FILE
  deskrealm-install bootstrap-ticket --config FILE

Preflight checks configuration, TLS materials, local Docker and host ports.
Prepare writes the persistent center identity and Compose configuration.
Install starts the deployment and checks its HTTPS and rendezvous services.
Status inspects the persisted deployment and running services.
Bootstrap-ticket prints a one-time initialization link for a ready center.
`

type portBinding struct {
	Service  string `json:"service"`
	Protocol string `json:"protocol"`
	Address  string `json:"address"`
}

type preflightResult struct {
	Directory      string        `json:"directory"`
	PublicOrigin   string        `json:"public_origin"`
	IDServer       string        `json:"id_server"`
	HTTPSMode      string        `json:"https_mode"`
	DockerPlatform string        `json:"docker_platform"`
	ComposeVersion string        `json:"compose_version"`
	Ports          []portBinding `json:"ports"`
}

type dockerRunner func(context.Context, ...string) ([]byte, error)

func docker(ctx context.Context, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", args...).Output()
}

func localDockerEndpoint(ctx context.Context, run dockerRunner) error {
	host := os.Getenv("DOCKER_HOST")
	if os.Getenv("DOCKER_CONTEXT") != "" || host == "" {
		data, err := run(ctx, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
		if err != nil {
			return errors.New("cannot inspect the selected Docker context")
		}
		host = strings.TrimSpace(string(data))
	}
	endpoint, err := url.Parse(host)
	if err != nil || endpoint.Scheme != "unix" || endpoint.Host != "" || !strings.HasPrefix(endpoint.Path, "/") {
		return errors.New("preflight requires a local Docker Unix socket to check host ports")
	}
	return nil
}

func checkRuntime(ctx context.Context, run dockerRunner) (platform, compose string, err error) {
	if err := localDockerEndpoint(ctx, run); err != nil {
		return "", "", err
	}
	engine, err := run(ctx, "version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
	if err != nil {
		return "", "", errors.New("Docker Engine is unavailable for the selected context")
	}
	platform = strings.TrimSpace(string(engine))
	if platform != "linux/amd64" && platform != "linux/arm64" {
		return "", "", errors.New("Docker Engine must run linux/amd64 or linux/arm64")
	}
	data, err := run(ctx, "compose", "version", "--short")
	compose = strings.TrimSpace(string(data))
	if err != nil || compose == "" {
		return "", "", errors.New("Docker Compose plugin is unavailable")
	}
	return platform, compose, nil
}

func check(ctx context.Context, c config, run dockerRunner) (preflightResult, error) {
	var result preflightResult
	platform, compose, err := checkRuntime(ctx, run)
	if err != nil {
		return result, err
	}
	apiHost := "127.0.0.1"
	if c.HTTPS.Mode == httpsProvided {
		apiHost = c.Ports.BindHost
	}
	bindings := []portBinding{
		{"api", "tcp", joinPort(apiHost, c.Ports.API)},
		{"rendezvous_check", "tcp", joinPort(c.Ports.BindHost, c.Ports.RendezvousCheck)},
		{"rendezvous", "tcp", joinPort(c.Ports.BindHost, c.Ports.Rendezvous)},
		{"rendezvous", "udp", joinPort(c.Ports.BindHost, c.Ports.Rendezvous)},
	}
	for _, binding := range bindings {
		if err := checkPort(ctx, binding); err != nil {
			return result, fmt.Errorf("%s port %s %s: %w", binding.Service, binding.Protocol, binding.Address, err)
		}
	}
	return preflightResult{
		Directory: c.Directory, PublicOrigin: c.PublicOrigin, IDServer: c.IDServer,
		HTTPSMode: c.HTTPS.Mode, DockerPlatform: platform, ComposeVersion: compose, Ports: bindings,
	}, nil
}

func joinPort(host string, port uint16) string {
	return net.JoinHostPort(host, strconv.Itoa(int(port)))
}

func checkPort(ctx context.Context, binding portBinding) error {
	var config net.ListenConfig
	if binding.Protocol == "udp" {
		socket, err := config.ListenPacket(ctx, "udp", binding.Address)
		if err != nil {
			return err
		}
		return socket.Close()
	}
	socket, err := config.Listen(ctx, "tcp", binding.Address)
	if err != nil {
		return err
	}
	return socket.Close()
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 1 && (args[0] == "help" || args[0] == "--help" || args[0] == "-h") {
		_, err := io.WriteString(stdout, usage)
		return err
	}
	if len(args) == 0 {
		return errors.New("expected a command with --config FILE; see --help")
	}
	switch args[0] {
	case "preflight", "prepare", "install", "status", "bootstrap-ticket":
	default:
		return errors.New("unknown installer command; see --help")
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	path := flags.String("config", "", "path to the center installation JSON")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errors.New("invalid installer flags; see --help")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	c, err := loadConfig(*path, time.Now())
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(stdout)
	encoder.SetIndent("", "  ")
	switch args[0] {
	case "prepare":
		result, err := prepareDeployment(c)
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	case "install":
		result, err := installCenter(ctx, c, docker)
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	case "status", "bootstrap-ticket":
		ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		center, err := readPrepared(c)
		if err != nil {
			return err
		}
		if _, _, err := checkRuntime(ctx, docker); err != nil {
			return err
		}
		if args[0] == "bootstrap-ticket" {
			ticket, err := issueBootstrapTicket(ctx, c, center, docker)
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(stdout, ticket)
			return err
		}
		result, err := inspectCenter(ctx, c, center, docker)
		if err != nil {
			return err
		}
		return encoder.Encode(result)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := check(ctx, c, docker)
	if err != nil {
		return err
	}
	return encoder.Encode(result)
}
