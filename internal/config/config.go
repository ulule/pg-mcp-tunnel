package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Targets map[string]*Target `yaml:"targets"`
}

type Target struct {
	SSH        *SSHConfig     `yaml:"ssh,omitempty"`
	Postgres   PostgresConfig `yaml:"postgres"`
	AccessMode string         `yaml:"access_mode"`
}

type SSHConfig struct {
	Host string `yaml:"host"`
	User string `yaml:"user"`
	Port int    `yaml:"port,omitempty"`
}

type PostgresConfig struct {
	URI        string `yaml:"uri,omitempty"`
	RemoteHost string `yaml:"remote_host,omitempty"`
	RemotePort int    `yaml:"remote_port,omitempty"`
	Database   string `yaml:"database,omitempty"`
	User       string `yaml:"user,omitempty"`
	Password   string `yaml:"password,omitempty"`
	SSLMode    string `yaml:"sslmode,omitempty"`
}

func LoadFile(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) Validate() error {
	if len(c.Targets) == 0 {
		return fmt.Errorf("no targets defined")
	}
	for name, t := range c.Targets {
		if err := t.validate(); err != nil {
			return fmt.Errorf("target %q: %w", name, err)
		}
	}
	return nil
}

func (t *Target) validate() error {
	switch t.AccessMode {
	case "", "unrestricted", "restricted":
	default:
		return fmt.Errorf("access_mode must be unrestricted or restricted, got %q", t.AccessMode)
	}
	if t.SSH != nil {
		if t.SSH.Host == "" {
			return fmt.Errorf("ssh.host is required")
		}
		if t.Postgres.URI != "" {
			return fmt.Errorf("postgres.uri is not allowed when ssh is set; use remote_host/remote_port/database/user")
		}
		if t.Postgres.RemoteHost == "" || t.Postgres.RemotePort == 0 || t.Postgres.Database == "" || t.Postgres.User == "" {
			return fmt.Errorf("postgres.remote_host, remote_port, database, user are required when ssh is set")
		}
	} else {
		if t.Postgres.URI == "" {
			return fmt.Errorf("postgres.uri is required when ssh is not set")
		}
	}
	return nil
}

func (t *Target) Mode() string {
	if t.AccessMode == "" {
		return "unrestricted"
	}
	return t.AccessMode
}

// BuildURI returns the DATABASE_URI to pass to postgres-mcp.
// If the target uses SSH, localPort must be the port of the established tunnel.
// Otherwise localPort is ignored and the configured URI is returned as-is.
func (t *Target) BuildURI(localPort int) string {
	if t.SSH == nil {
		return t.Postgres.URI
	}
	u := url.URL{
		Scheme: "postgresql",
		Host:   "127.0.0.1:" + strconv.Itoa(localPort),
		Path:   "/" + t.Postgres.Database,
	}
	if t.Postgres.User != "" {
		if t.Postgres.Password != "" {
			u.User = url.UserPassword(t.Postgres.User, t.Postgres.Password)
		} else {
			u.User = url.User(t.Postgres.User)
		}
	}
	q := u.Query()
	if t.Postgres.SSLMode != "" {
		q.Set("sslmode", t.Postgres.SSLMode)
	}
	u.RawQuery = q.Encode()
	return u.String()
}
