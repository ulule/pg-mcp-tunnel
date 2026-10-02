package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoadValidDirectURI(t *testing.T) {
	path := writeYAML(t, `
targets:
  local:
    postgres:
      uri: postgresql://u:p@localhost:5432/db?sslmode=disable
    access_mode: unrestricted
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	tg := cfg.Targets["local"]
	if tg == nil {
		t.Fatal("missing target local")
	}
	if got := tg.BuildURI(0); got != "postgresql://u:p@localhost:5432/db?sslmode=disable" {
		t.Errorf("BuildURI = %q", got)
	}
}

func TestLoadValidSSH(t *testing.T) {
	path := writeYAML(t, `
targets:
  prod:
    ssh:
      host: db.example.com
      user: alice
    postgres:
      remote_host: localhost
      remote_port: 5432
      database: mydb
      user: alice
      sslmode: disable
    access_mode: restricted
`)
	cfg, err := LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	tg := cfg.Targets["prod"]
	got := tg.BuildURI(54321)
	want := "postgresql://alice@127.0.0.1:54321/mydb?connect_timeout=30&sslmode=disable"
	if got != want {
		t.Errorf("BuildURI = %q, want %q", got, want)
	}
	if tg.Mode() != "restricted" {
		t.Errorf("Mode = %q", tg.Mode())
	}
}

func TestValidateErrors(t *testing.T) {
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "no targets",
			yaml: `targets: {}`,
			want: "no targets",
		},
		{
			name: "ssh without remote pg fields",
			yaml: `
targets:
  bad:
    ssh: {host: h, user: u}
    postgres: {}
`,
			want: "remote_host",
		},
		{
			name: "no ssh and no uri",
			yaml: `
targets:
  bad:
    postgres: {}
`,
			want: "postgres.uri is required",
		},
		{
			name: "ssh and uri together",
			yaml: `
targets:
  bad:
    ssh: {host: h, user: u}
    postgres:
      uri: postgresql://x/y
      remote_host: localhost
      remote_port: 5432
      database: d
      user: u
`,
			want: "not allowed",
		},
		{
			name: "bad access_mode",
			yaml: `
targets:
  bad:
    postgres: {uri: "postgresql://x/y"}
    access_mode: yolo
`,
			want: "access_mode",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeYAML(t, tc.yaml)
			_, err := LoadFile(path)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestDirectAddrAndRelayURI(t *testing.T) {
	cases := []struct {
		uri      string
		wantAddr string // "" = not relayable
	}{
		{"postgresql://u:p@localhost:5432/db?sslmode=disable", "localhost:5432"},
		{"postgres://u@db.internal/db?sslmode=disable", "db.internal:5432"},
		{"postgresql://u@[::1]:6543/db?sslmode=disable", "[::1]:6543"},
		{"postgresql://u@localhost/db", ""},                       // default sslmode=prefer
		{"postgresql://u@localhost/db?sslmode=verify-full", ""},   // TLS
		{"postgresql:///db?sslmode=disable", ""},                  // unix socket
		{"postgresql://u@/db?host=/tmp&sslmode=disable", ""},      // unix socket
		{"postgresql://u@h1:5432,h2:5432/db?sslmode=disable", ""}, // multi-host
		{"host=localhost dbname=db sslmode=disable", ""},          // keyword DSN
	}
	for _, c := range cases {
		tg := &Target{Postgres: PostgresConfig{URI: c.uri}}
		addr, err := tg.DirectAddr()
		if c.wantAddr == "" {
			if err == nil {
				t.Errorf("%s: want not relayable, got %q", c.uri, addr)
			}
			continue
		}
		if err != nil || addr != c.wantAddr {
			t.Errorf("%s: got %q, %v; want %q", c.uri, addr, err, c.wantAddr)
		}
	}

	tg := &Target{Postgres: PostgresConfig{URI: "postgresql://u:p%40ss@localhost:5432/db?sslmode=disable"}}
	if got, want := tg.BuildURI(4242), "postgresql://u:p%40ss@127.0.0.1:4242/db?sslmode=disable"; got != want {
		t.Errorf("BuildURI(4242) = %q, want %q", got, want)
	}
	if got := tg.BuildURI(0); got != tg.Postgres.URI {
		t.Errorf("BuildURI(0) = %q, want original", got)
	}
}
