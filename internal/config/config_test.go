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
	want := "postgresql://alice@127.0.0.1:54321/mydb?sslmode=disable"
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
