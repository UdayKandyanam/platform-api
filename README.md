# Platform API

A small self-service API for Kubernetes-style workloads, written in Go with only the standard library.

The pattern this project demonstrates is the one I use when application teams need lifecycle operations on a platform without getting raw cluster access. Teams call a small API with their own key. The API checks who they are, checks that the namespace belongs to them, and then creates, scales, or deletes deployments on their behalf. Every mutating call is written to an audit log, and every mutating call can be run as a dry run first.

v1 runs against an in-memory simulated cluster behind a clean interface, so the API, the policy checks, and the tests are real while the backend stays swappable. A Kubernetes backend can replace the in-memory one without changing a single endpoint.

## Architecture

```text
request
  -> auth: X-API-Key maps to a team and a role (admin or team)
  -> policy: a team may only mutate namespaces prefixed with its team name
  -> dry run or apply: ?dry_run=true validates and reports, changes nothing
  -> audit: every mutating call is logged with actor, target, and result
  -> cluster backend: the Cluster interface (in-memory today)
```

Layout:

- `internal/cluster`: the `Cluster` interface and the in-memory implementation. Typed errors (`ErrNotFound`, `ErrConflict`, `ErrInvalid`) keep storage details out of the HTTP layer.
- `internal/api`: the HTTP server, authentication, tenancy policy, and error mapping (404, 409, 400, 401, 403).
- `internal/audit`: an append-only audit log, in memory plus an optional JSONL file.
- `cmd/platform-api`: flags and startup. That is all it does.

## Quick start

Build:

```bash
go build -o platform-api ./cmd/platform-api
```

Write a keys file from the example. The example ships with two invented teams (checkout and storefront) and one admin key:

```bash
cp keys.example.json keys.json
```

Run:

```bash
./platform-api --addr :8080 --keys keys.json --audit-log audit.jsonl
# 2026/11/13 20:04:09 platform-api listening on :8080 with 3 keys
```

Health is open, everything else needs a key:

```bash
curl -s http://localhost:8080/health
# {"status":"ok"}

curl -s http://localhost:8080/namespaces
# {"error":"missing or unknown API key"}   (401)
```

Create a namespace and a deployment as the checkout team:

```bash
curl -s -X POST http://localhost:8080/namespaces \
  -H "X-API-Key: dev-key-checkout-001" -d '{"name":"checkout-prod"}'
# {"namespace":"checkout-prod"}   (201)

curl -s -X POST http://localhost:8080/namespaces/checkout-prod/deployments \
  -H "X-API-Key: dev-key-checkout-001" \
  -d '{"name":"checkout-api","image":"checkout-api:1.4.0","replicas":2}'
# {"name":"checkout-api","namespace":"checkout-prod","image":"checkout-api:1.4.0","replicas":2}   (201)
```

Dry run a second deployment. It validates and reports, and changes nothing:

```bash
curl -s -X POST "http://localhost:8080/namespaces/checkout-prod/deployments?dry_run=true" \
  -H "X-API-Key: dev-key-checkout-001" \
  -d '{"name":"checkout-worker","image":"checkout-worker:0.9.0","replicas":1}'
# {"dry_run":true,"would_create":{"name":"checkout-worker","namespace":"checkout-prod","image":"checkout-worker:0.9.0","replicas":1}}   (200)

curl -s http://localhost:8080/namespaces/checkout-prod/deployments \
  -H "X-API-Key: dev-key-checkout-001"
# {"deployments":[{"name":"checkout-api","namespace":"checkout-prod","image":"checkout-api:1.4.0","replicas":2}]}
```

A team cannot touch another team's namespace:

```bash
curl -s -X POST http://localhost:8080/namespaces/checkout-prod/deployments \
  -H "X-API-Key: dev-key-storefront-001" \
  -d '{"name":"rogue","image":"rogue:1","replicas":1}'
# {"error":"team storefront can only manage namespaces prefixed with storefront-"}   (403)
```

Scale and delete:

```bash
curl -s -X POST http://localhost:8080/namespaces/checkout-prod/deployments/checkout-api/scale \
  -H "X-API-Key: dev-key-checkout-001" -d '{"replicas":0}'

curl -s -X DELETE http://localhost:8080/namespaces/checkout-prod/deployments/checkout-api \
  -H "X-API-Key: dev-key-checkout-001"
```

Every mutating call above landed in the audit log, including the dry run and the denied one:

```json
{"actor":"checkout","action":"create_namespace","target":"checkout-prod","dry_run":false,"result":"created"}
{"actor":"checkout","action":"create_deployment","target":"checkout-prod/checkout-api","dry_run":false,"result":"created"}
{"actor":"checkout","action":"create_deployment","target":"checkout-prod/checkout-worker","dry_run":true,"result":"dry_run"}
{"actor":"storefront","action":"create_deployment","target":"checkout-prod/rogue","dry_run":false,"result":"denied"}
```

(Timestamps are in the real file; they are trimmed here to keep the lines short.)

## Endpoints

| Method | Path | Notes |
| --- | --- | --- |
| GET | /health | Open, no key needed |
| GET | /namespaces | Any valid key |
| POST | /namespaces | Body: `{"name": "..."}` |
| DELETE | /namespaces/{name} | Deletes the namespace and its deployments |
| GET | /namespaces/{ns}/deployments | Any valid key |
| POST | /namespaces/{ns}/deployments | Body: `{"name", "image", "replicas"}` |
| POST | /namespaces/{ns}/deployments/{name}/scale | Body: `{"replicas": n}` |
| DELETE | /namespaces/{ns}/deployments/{name} | |

All mutating endpoints accept `?dry_run=true`. Errors come back as `{"error": "..."}` with a status that matches the problem: 400 for invalid input, 401 for a missing or unknown key, 403 for a tenancy violation, 404 for a missing namespace or deployment, 409 for a duplicate create.

## Design choices

- Interface first: the HTTP layer depends on a `Cluster` interface, not on the in-memory store. The backend is the one part that is supposed to change.
- Prefix tenancy: a team key can only mutate namespaces named after its team (checkout owns checkout-prod, checkout-staging, and so on). The rule is one string check, easy to explain, easy to audit, and it maps cleanly onto how namespace naming conventions already work.
- Audit everything: creates, scales, deletes, dry runs, and denied calls are all recorded with the actor and the outcome. A denied call is exactly the record you want later.
- Standard library only: `net/http` with the 1.22+ routing patterns does everything this API needs. No framework, no dependencies, builds anywhere Go builds.

## Limits

- State is in memory. Restart the process and the cluster is empty. That is fine for demonstrating the API pattern and for tests, and it is the first thing a real backend removes.
- Authentication is API keys from a local JSON file. Good enough to demonstrate per-team identity and roles, not a substitute for real identity federation.
- Replica counts are capped at 50 and names follow DNS label rules. The caps exist so invalid input fails early with a clear error.

## Roadmap

1. A Kubernetes backend through client-go, implementing the same `Cluster` interface against a kind cluster.
2. OIDC authentication in place of static API keys, with roles read from token claims.
3. A small dashboard page listing namespaces, deployments, and the recent audit trail.

## Tests

```bash
go test ./...
```

Thirteen tests over HTTP against the in-memory backend: auth open and closed paths, the happy path, both tenancy denials, dry run behavior, scale to zero and back, deletes, conflicts, not found, invalid bodies, and the audit trail (a denied call and a dry run, recorded with actor and flags).

## License

MIT, see LICENSE.
