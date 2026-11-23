package install

import (
	"encoding/json"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
)

type composeConfig struct {
	Name     string                    `json:"name"`
	Services map[string]composeService `json:"services"`
	Networks map[string]composeNetwork `json:"networks"`
}

type composeNetwork struct {
	Internal bool `json:"internal,omitempty"`
}

type composeAttachment struct {
	Aliases []string `json:"aliases,omitempty"`
}

type composeDependency struct {
	Condition string `json:"condition"`
}

type composeHealth struct {
	Test        []string `json:"test"`
	Interval    string   `json:"interval"`
	Timeout     string   `json:"timeout"`
	Retries     int      `json:"retries"`
	StartPeriod string   `json:"start_period"`
}

type composeService struct {
	Image       string                       `json:"image"`
	User        string                       `json:"user,omitempty"`
	Entrypoint  []string                     `json:"entrypoint,omitempty"`
	Command     []string                     `json:"command,omitempty"`
	WorkingDir  string                       `json:"working_dir,omitempty"`
	Environment map[string]string            `json:"environment,omitempty"`
	EnvFile     []string                     `json:"env_file,omitempty"`
	Ports       []composePort                `json:"ports,omitempty"`
	Volumes     []composeBind                `json:"volumes,omitempty"`
	Networks    map[string]composeAttachment `json:"networks"`
	DependsOn   map[string]composeDependency `json:"depends_on,omitempty"`
	Healthcheck *composeHealth               `json:"healthcheck,omitempty"`
	Restart     string                       `json:"restart,omitempty"`
}

type composePort struct {
	Target    uint16 `json:"target"`
	Published string `json:"published"`
	Protocol  string `json:"protocol"`
	HostIP    string `json:"host_ip,omitempty"`
}

type composeBind struct {
	Type     string `json:"type"`
	Source   string `json:"source"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"read_only,omitempty"`
	Bind     struct {
		CreateHostPath bool `json:"create_host_path"`
	} `json:"bind"`
}

func bind(source, target string, readonly bool) composeBind {
	// Compose applies variable interpolation to values in JSON as well.
	return composeBind{Type: "bind", Source: strings.ReplaceAll(source, "$", "$$"), Target: target, ReadOnly: readonly}
}

func port(public, target uint16, protocol, host string) composePort {
	return composePort{Target: target, Published: strconv.Itoa(int(public)), Protocol: protocol, HostIP: host}
}

func renderCompose(d deployment) ([]byte, error) {
	c := d.Config
	centerImage := []string{"/usr/local/bin/deskrealm"}
	db := composeService{
		Image:       c.Images.PostgreSQL,
		Environment: map[string]string{"POSTGRES_DB": "deskrealm", "POSTGRES_USER": "postgres"},
		EnvFile:     []string{"./secrets/postgres.env"},
		Volumes: []composeBind{
			bind(filepath.Join(c.Directory, "postgres"), "/var/lib/postgresql", false),
			bind(filepath.Join(c.Directory, "postgres-init.sql"), "/docker-entrypoint-initdb.d/001-deskrealm.sql", true),
		},
		Networks: map[string]composeAttachment{"database": {}},
		Healthcheck: &composeHealth{
			Test:     []string{"CMD", "pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "deskrealm"},
			Interval: "2s", Timeout: "3s", Retries: 30, StartPeriod: "15s",
		},
		Restart: "unless-stopped",
	}
	migrate := composeService{
		Image: c.Images.Center, User: d.RunAs, Entrypoint: centerImage, Command: []string{"migrate"},
		EnvFile:   []string{"./secrets/migrate.env"},
		Networks:  map[string]composeAttachment{"database": {}},
		DependsOn: map[string]composeDependency{"database": {Condition: "service_healthy"}},
	}
	center := composeService{
		Image: c.Images.Center, User: d.RunAs, Entrypoint: centerImage, Command: []string{"serve"},
		Environment: map[string]string{
			"DESKREALM_PUBLIC_ORIGIN":            c.PublicOrigin,
			"DESKREALM_HTTP_ADDR":                "0.0.0.0:8080",
			"DESKREALM_WEB_DIR":                  "/opt/deskrealm/web",
			"DESKREALM_SESSION_SIGNING_KEY_FILE": "/run/deskrealm/signing.seed",
			"DESKREALM_SESSION_SIGNING_KID":      d.Center.SigningKeyID,
		},
		EnvFile:   []string{"./secrets/center.env"},
		Ports:     []composePort{port(c.Ports.API, 8080, "tcp", "127.0.0.1")},
		Volumes:   []composeBind{bind(filepath.Join(c.Directory, "secrets", "signing.seed"), "/run/deskrealm/signing.seed", true)},
		Networks:  map[string]composeAttachment{"database": {}, "control": {}},
		DependsOn: map[string]composeDependency{"migrate": {Condition: "service_completed_successfully"}},
		Restart:   "unless-stopped",
	}
	if c.HTTPS.Mode == httpsProvided {
		center.Ports = []composePort{port(c.Ports.API, c.Ports.API, "tcp", c.Ports.BindHost)}
		center.Environment["DESKREALM_HTTP_ADDR"] = joinPort("0.0.0.0", c.Ports.API)
		center.Environment["DESKREALM_TLS_CERT_FILE"] = "/run/deskrealm/tls.crt"
		center.Environment["DESKREALM_TLS_KEY_FILE"] = "/run/deskrealm/tls.key"
		center.Volumes = append(center.Volumes, bind(c.HTTPS.Certificate, "/run/deskrealm/tls.crt", true),
			bind(c.HTTPS.PrivateKey, "/run/deskrealm/tls.key", true))
		origin, err := url.Parse(c.PublicOrigin)
		if err != nil {
			return nil, err
		}
		if _, err := netip.ParseAddr(origin.Hostname()); err != nil {
			center.Networks["control"] = composeAttachment{Aliases: []string{origin.Hostname()}}
		}
	}
	rendezvous := composeService{
		Image: c.Images.Rendezvous, User: d.RunAs, Entrypoint: []string{"/usr/local/bin/hbbs"},
		Command: []string{"-k", "_"}, WorkingDir: "/var/lib/deskrealm-rendezvous",
		Environment: map[string]string{
			"DESKREALM_MANAGED_MODE":              "1",
			"DESKREALM_MANAGED_CENTER":            c.PublicOrigin,
			"DESKREALM_MANAGED_CENTER_KID":        d.Center.SigningKeyID,
			"DESKREALM_MANAGED_CENTER_PUBLIC_KEY": d.Center.SigningKey,
		},
		EnvFile: []string{"./secrets/rendezvous.env"},
		Ports: []composePort{
			port(c.Ports.RendezvousCheck, 21115, "tcp", c.Ports.BindHost),
			port(c.Ports.Rendezvous, 21116, "tcp", c.Ports.BindHost),
			port(c.Ports.Rendezvous, 21116, "udp", c.Ports.BindHost),
		},
		Volumes:   []composeBind{bind(filepath.Join(c.Directory, "rendezvous"), "/var/lib/deskrealm-rendezvous", false)},
		Networks:  map[string]composeAttachment{"control": {}},
		DependsOn: map[string]composeDependency{"center": {Condition: "service_started"}},
		Restart:   "unless-stopped",
	}
	if c.HTTPS.CACertificate != "" {
		rendezvous.Environment["DESKREALM_MANAGED_CA_FILE"] = "/run/deskrealm/center-ca.pem"
		rendezvous.Volumes = append(rendezvous.Volumes,
			bind(c.HTTPS.CACertificate, "/run/deskrealm/center-ca.pem", true))
	}
	return json.MarshalIndent(composeConfig{
		Name:     d.Center.Project,
		Services: map[string]composeService{"database": db, "migrate": migrate, "center": center, "rendezvous": rendezvous},
		Networks: map[string]composeNetwork{"database": {Internal: true}, "control": {}},
	}, "", "  ")
}
