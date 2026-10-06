<p align="center">
  <img src="assets/logo.png" alt="pg-mcp-tunnel" width="280" />
</p>

# pg-mcp-tunnel

Serveur MCP Postgres pour Claude, avec tunnel SSH intégré et dashboard des requêtes en direct. Un seul binaire Go.

## Fonctionnalités

- **Tunnel SSH intégré** : Claude interroge une base joignable uniquement en SSH, sans `ssh -L` ni script à lancer à la main.
- **7 outils MCP** : exploration du schéma, exécution SQL, `EXPLAIN` (avec index hypothétiques), requêtes les plus lentes, bilan de santé.
- **Mode `restricted` sécurisé** pour la prod : chaque requête est analysée par le vrai parser Postgres, puis exécutée dans une transaction en lecture seule avec un timeout.
- **Dashboard** : flux des requêtes en direct par base, top des requêtes, et estimation des tokens que chaque appel coûte au modèle.
- **Un seul binaire** : pas de Python, pas de `uv`, pas de cgo.
- **Compatible** avec les permissions Claude existantes (`mcp__<cible>__execute_sql`, …).

## Installation

Prérequis :

- Go ≥ 1.25 ;
- un accès au repo `ulule/pg-mcp-tunnel` (privé). Indiquer à Go de ne pas passer par le proxy public :

```bash
go env -w GOPRIVATE='github.com/ulule/*'
```

Installer la dernière version :

```bash
go install github.com/ulule/pg-mcp-tunnel/cmd/pg-mcp-tunnel@main
```

Ou depuis un clone du repo :

```bash
make install
```

Le binaire arrive dans `~/go/bin/pg-mcp-tunnel`. Redémarrer ensuite Claude (Code et Desktop) : il relance les serveurs MCP avec le nouveau binaire.

## Configuration

Les cibles se déclarent dans `~/.config/pg-mcp-tunnel/config.yaml` (exemple complet : [`examples/config.yaml`](examples/config.yaml)) :

```yaml
targets:
  # Base locale, connexion directe.
  local:
    postgres:
      uri: postgresql://user:pass@localhost:5432/mydb?sslmode=disable
    access_mode: unrestricted

  # Base distante, via SSH, en lecture seule.
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
    allowed_functions: [unaccent]
```

| Champ | Description |
|---|---|
| `ssh.host`, `ssh.user`, `ssh.port` | Serveur SSH. L'authentification passe par OpenSSH : `ssh-agent`, `~/.ssh/config`, `ProxyJump`, clés FIDO, trousseau macOS. |
| `postgres.uri` | URI complète, pour une cible sans SSH. |
| `postgres.remote_host`, `remote_port`, `database`, `user`, `password`, `sslmode` | Connexion vue depuis le serveur SSH, pour une cible avec SSH. |
| `access_mode` | `unrestricted` (par défaut) ou `restricted`. Voir [Modes d'accès](#modes-daccès). |
| `allowed_functions` | Fonctions en lecture seule à autoriser en plus en mode `restricted` (ex. `unaccent`, `public.ma_fonction`). |

## Utilisation avec Claude

Claude Code, dans `.mcp.json` :

```json
{
  "mcpServers": {
    "local": { "command": "pg-mcp-tunnel", "args": ["--target", "local"] },
    "prod":  { "command": "pg-mcp-tunnel", "args": ["--target", "prod"] }
  }
}
```

Claude Desktop : même bloc `mcpServers` dans `~/Library/Application Support/Claude/claude_desktop_config.json`.

| Option | Défaut | Rôle |
|---|---|---|
| `--target` | (obligatoire) | Nom de la cible dans `config.yaml`. |
| `--config` | `~/.config/pg-mcp-tunnel/config.yaml` | Fichier de configuration. |
| `--log-file` | `~/.cache/pg-mcp-tunnel/<cible>.log` | Logs du serveur et de ssh. |
| `--record-queries` | `true` | Enregistrer les requêtes pour le dashboard. |

## Outils

| Outil | Paramètres | Rôle |
|---|---|---|
| `list_schemas` | – | Liste les schémas. |
| `list_objects` | `schema_name`, `object_type` (`table`, `view`, `sequence`, `extension`) | Liste les objets d'un schéma. |
| `get_object_details` | `schema_name`, `object_name`, `object_type` | Colonnes, contraintes et index d'un objet. |
| `execute_sql` | `sql` | Exécute du SQL et renvoie les lignes en JSON. Plusieurs requêtes possibles : seul le dernier résultat est renvoyé. |
| `explain_query` | `sql`, `analyze`, `hypothetical_indexes` | Plan d'exécution lisible. Les index hypothétiques nécessitent l'extension `hypopg`. Les paramètres `$1` utilisent un plan générique (PG 16+). |
| `get_top_queries` | `sort_by` (`resources`, `mean_time`, `total_time`), `limit` | Requêtes les plus coûteuses, via `pg_stat_statements`. |
| `analyze_db_health` | `health_type` (`all`, `index`, `connection`, `vacuum`, `sequence`, `replication`, `buffer`, `constraint`) | Bilan de santé de la base. |

## Modes d'accès

### `unrestricted`

Tout est permis : lecture, écriture, DDL, en autocommit. `explain_query` avec `analyze` exécute vraiment la requête, mais dans une transaction annulée ensuite : un `UPDATE` analysé n'est pas enregistré.

### `restricted`

Deux protections successives :

1. **Analyse de la requête.** Le SQL passe par le vrai parser Postgres (libpg_query, embarqué en WASM) et chaque nœud est vérifié contre une liste blanche. Sont refusés, avec un message qui dit pourquoi :
   - tout ce qui n'est pas une lecture : `INSERT`, `UPDATE`, `DELETE`, `TRUNCATE`, DDL, `COPY`, `SET`, `BEGIN`/`COMMIT` ;
   - les écritures cachées : CTE qui modifient des données, `SELECT … INTO` ;
   - `SELECT … FOR UPDATE` et `EXPLAIN ANALYZE` ;
   - les fonctions hors liste blanche, environ 490 fonctions de lecture étant autorisées (pas de `pg_sleep`, `set_config`, `pg_terminate_backend`, `pg_read_file`, `dblink`…). `allowed_functions` permet d'en ajouter.
2. **Transaction en lecture seule.** Chaque appel s'exécute dans `BEGIN TRANSACTION READ ONLY`, avec `statement_timeout` à 30 secondes, puis `ROLLBACK`.

Pour la prod, utiliser en plus un rôle Postgres en lecture seule.

## Dashboard

```bash
pg-mcp-tunnel ui --open        # http://localhost:7878
```

Chaque `pg-mcp-tunnel --target …` enregistre ses requêtes dans `/tmp/pg-mcp-tunnel-<uid>/<cible>.queries.jsonl`. Le dashboard lit ces fichiers et affiche :

- **Live** : les requêtes en direct, avec durée, nombre de lignes, erreurs, et le détail au clic ;
- **Top queries** : les requêtes regroupées par forme (littéraux remplacés par `?`), triables par nombre d'appels, temps total, moyen, max ou tokens ;
- **par base** (colonne de gauche) : tunnel actif, connexions ouvertes, débit, moyenne et p95, erreurs, tokens ;
- **les appels MCP** de Claude, avec une **estimation des tokens** (environ 3,5 caractères par token) envoyés au modèle. Pratique pour repérer le `SELECT *` qui a consommé 50 000 tokens de contexte ;
- **⚙ config.yaml** : la configuration, mots de passe masqués, avec un bouton pour l'ouvrir dans l'éditeur.

Filtres : base, texte, erreurs, requêtes lentes (≥ 100 ms), bruit (`BEGIN`/`COMMIT`/`SET`), appels MCP, connexions. Ils sont dans l'URL : un rechargement ou un favori retrouve la même vue. Raccourcis : `/` pour filtrer, `espace` pour mettre en pause.

À savoir :

- les requêtes ne sont visibles que si la connexion n'est pas chiffrée entre le serveur et le relais local : toujours le cas via SSH, et en direct seulement avec `sslmode=disable`. Les connexions TLS apparaissent comme opaques ;
- les fichiers contiennent le texte SQL (éventuellement avec des valeurs littérales). Ils sont en `0600`, limités à 20 Mo, et effacés au redémarrage de la machine. Pour désactiver l'enregistrement : `--record-queries=false` ;
- le dashboard n'accepte que les requêtes adressées à `localhost` ou `127.0.0.1`. Ses options : `--addr` (défaut `127.0.0.1:7878`), `--config`, `--open`.

## Fonctionnement

```
Claude ──stdio──▶ pg-mcp-tunnel (serveur MCP + pool pgx)
                        │
                        │ net.Listen           le pool se connecte
                        ▼                      à 127.0.0.1:LOCAL
                127.0.0.1:LOCAL
                        │
                        │ proxy TCP (Go), qui enregistre les requêtes
                        ▼
                127.0.0.1:INTERNE ◀──ssh -L──▶ serveur:5432
                        ▲
                        │ shepherd (sh + pipe sur FD 3) :
                        │ tue ssh si pg-mcp-tunnel meurt, quelle qu'en soit la cause
```

- **Démarrage immédiat.** Le port local est ouvert en quelques millisecondes et le serveur MCP répond tout de suite ; ssh se connecte en parallèle. Les premières connexions attendent simplement que le tunnel soit prêt (jusqu'à 30 s).
- **Pas de processus ssh orphelin.** ssh tourne sous un petit shell qui surveille un pipe : si pg-mcp-tunnel s'arrête (normalement, crash, `kill -9`, Claude quitté brutalement), ssh est arrêté aussi.
- **Erreurs SSH visibles.** Si ssh échoue ou ne monte pas en 30 s, la fin de sa sortie d'erreur est conservée. Le serveur reste actif et chaque appel d'outil répond `Error: ssh tunnel failed: <raison>`, que Claude peut lire.
- **Reconnexion automatique.** Si le lien tombe (veille, changement de Wi-Fi ou de VPN, timeout du bastion), l'appel d'outil suivant relance ssh sur le même port local. Pas besoin de reconnecter le serveur MCP. Après un échec de connexion, ssh n'est pas relancé pendant 5 s.

## Migration depuis la version Python

Les versions précédentes lançaient `postgres-mcp` (Python) via `uv`. Ce n'est plus nécessaire :

- installer la nouvelle version (voir [Installation](#installation)) et redémarrer Claude ;
- la configuration, les `.mcp.json` et les permissions Claude ne changent pas ;
- `postgres-mcp` et `uv` peuvent être désinstallés (`pip uninstall postgres-mcp`), et les variables `PYENV_VERSION` retirées des `.mcp.json` ;
- les résultats sont désormais en JSON.

## Dépannage

**`Killed: 9` au lancement (macOS).** Le binaire a été écrasé sur place, et macOS rejette son ancienne signature. Supprimer le fichier avant de réinstaller :

```bash
rm "$(which pg-mcp-tunnel)"
```

```bash
go install github.com/ulule/pg-mcp-tunnel/cmd/pg-mcp-tunnel@main
```

Ou re-signer le binaire en place :

```bash
codesign -s - -f "$(which pg-mcp-tunnel)"
```

**Le serveur MCP ne répond pas.** Lire les logs de la cible :

```bash
tail -50 ~/.cache/pg-mcp-tunnel/<cible>.log
```

**`ssh tunnel failed: …`.** Le tunnel est relancé à chaque appel : si l'erreur persiste, tester la connexion à la main avec `ssh <user>@<host>` : clé absente de l'agent, VPN, `~/.ssh/config`…

**`function … is not allowed in restricted mode`.** La fonction n'est pas dans la liste blanche. Si elle est en lecture seule, l'ajouter à `allowed_functions` dans la cible, puis redémarrer Claude.

**Vieux processus qui traînent.** Des `pg-mcp-tunnel --target …` d'anciennes sessions peuvent rester actifs. Quitter Claude, puis :

```bash
pkill -f 'pg-mcp-tunnel --target'
```

## Développement

```bash
make build      # ./pg-mcp-tunnel
make test       # go test -race ./...
make lint       # golangci-lint (config : .golangci.yml)
```

La CI GitHub Actions (`.github/workflows/ci.yml`) lance sur chaque push sur `main` et chaque PR : `go mod tidy -diff`, golangci-lint, puis les tests (avec `-race`) contre un Postgres 17 en service container, donc tests d'intégration compris.

Les tests d'intégration ont besoin d'une base Postgres jetable :

```bash
createdb pgmcp_test
```

```bash
PGMCP_TEST_DATABASE_URL='postgresql://localhost/pgmcp_test?sslmode=disable' make test
```

Organisation du code :

| Paquet | Rôle |
|---|---|
| `cmd/pg-mcp-tunnel` | Point d'entrée : serveur MCP (`--target`) et dashboard (`ui`). |
| `internal/config` | Lecture et validation de `config.yaml`. |
| `internal/sshtunnel`, `internal/tcpproxy` | Tunnel SSH asynchrone et relais local. |
| `internal/pgmcp` | Serveur MCP et outils. Sous-paquets : `pgdb` (accès Postgres), `safesql` (validation du mode `restricted`), `explain`, `health`, `topqueries`. |
| `internal/querylog` | Lecture du protocole Postgres et enregistrement des requêtes. |
| `internal/webui` | Dashboard (rendu côté serveur, htmx embarqué). |

## Licence

MIT
