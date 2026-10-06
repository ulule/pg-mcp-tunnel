package webui

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"runtime"
)

// Secrets in the config are masked before being shown: password values and
// the password part of postgres URIs.
var (
	passwordLine  = regexp.MustCompile(`(?m)^(\s*(?:-\s*)?password\s*:\s*)(\S.*)$`)
	uriCredential = regexp.MustCompile(`(postgres(?:ql)?://[^:@/\s]+:)[^@\s]+@`)
)

func maskSecrets(cfg string) string {
	cfg = passwordLine.ReplaceAllString(cfg, `${1}"***"`)
	return uriCredential.ReplaceAllString(cfg, `${1}***@`)
}

var configPage = template.Must(template.New("config").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>pg-mcp-tunnel · config</title>
<style>
:root { --bg: #f6f7f9; --panel: #fff; --border: #e2e5ea; --text: #1b1f24; --muted: #6a7280; --accent: #3b6fd8; --ok: #1f8a4c; --err: #c53030;
  --mono: ui-monospace, SFMono-Regular, "JetBrains Mono", Menlo, Consolas, monospace; --sans: system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; }
@media (prefers-color-scheme: dark) { :root { --bg: #0f1115; --panel: #161920; --border: #262b36; --text: #e4e7ec; --muted: #8b93a1; --accent: #6c9cff; --ok: #4cc38a; --err: #f87171; } }
body { margin: 0; background: var(--bg); color: var(--text); font: 13px/1.5 var(--sans); }
main { max-width: 900px; margin: 0 auto; padding: 20px 16px; }
a { color: var(--accent); text-decoration: none; }
h1 { font-size: 16px; margin: 14px 0 4px; }
.path { font: 12px var(--mono); color: var(--muted); word-break: break-all; }
.bar { display: flex; flex-wrap: wrap; gap: 8px; align-items: center; margin: 14px 0; }
.btn { border: 1px solid var(--border); background: var(--panel); color: var(--text); border-radius: 7px; padding: 5px 11px; font: inherit; cursor: pointer; }
.btn:hover { border-color: var(--accent); }
#msg { color: var(--muted); }
#msg.ok { color: var(--ok); } #msg.err { color: var(--err); }
pre { background: var(--panel); border: 1px solid var(--border); border-radius: 8px; padding: 12px 14px; font: 12px/1.55 var(--mono); overflow-x: auto; margin: 0; }
.note { color: var(--muted); margin-top: 8px; font-size: 12px; }
</style>
</head>
<body>
<main>
  <a href="./">← live queries</a>
  <h1>config.yaml</h1>
  <div class="path">{{.Path}}</div>
  <div class="bar">
    <button class="btn" id="open">Open in editor</button>
    <button class="btn" id="copy">Copy path</button>
    <span id="msg"></span>
  </div>
  {{if .Err}}<pre>{{.Err}}</pre>{{else}}<pre>{{.Content}}</pre>
  <div class="note">Passwords are masked. Edit the file itself; running tunnels pick up changes when Claude restarts them.</div>{{end}}
</main>
<script>
const msg = (text, cls) => { const m = document.getElementById("msg"); m.textContent = text; m.className = cls || ""; };
document.getElementById("copy").onclick = () =>
  navigator.clipboard.writeText({{.Path}}).then(() => msg("Path copied ✓", "ok"));
document.getElementById("open").onclick = async () => {
  const r = await fetch("config/open", { method: "POST" });
  r.ok ? msg("Opened ✓", "ok") : msg(await r.text(), "err");
};
</script>
</body>
</html>`))

func (s *Server) handleConfig(w http.ResponseWriter, _ *http.Request) {
	data := struct{ Path, Content, Err string }{Path: s.configPath}
	if s.configPath == "" {
		data.Err = "no config path"
	} else if b, err := os.ReadFile(s.configPath); err != nil {
		data.Err = err.Error()
	} else {
		data.Content = maskSecrets(string(b))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = configPage.Execute(w, data)
}

// handleOpenConfig opens the config file in the desktop's default editor.
func (s *Server) handleOpenConfig(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if _, err := os.Stat(s.configPath); err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	if err := openInEditor(s.configPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// sameOrigin guards state-changing requests: localOnly checks the Host
// header (DNS rebinding), but a page on another site can still POST to
// localhost, and its Origin gives it away.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func openInEditor(path string) error {
	switch runtime.GOOS {
	case "darwin":
		// Default app for .yaml files, falling back to the default text editor.
		if exec.Command("open", path).Run() == nil {
			return nil
		}
		return exec.Command("open", "-t", path).Run()
	case "linux":
		return exec.Command("xdg-open", path).Start()
	default:
		return errors.New("opening files is not supported on " + runtime.GOOS)
	}
}
