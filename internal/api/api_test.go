package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"platform-api/internal/api"
	"platform-api/internal/audit"
	"platform-api/internal/cluster"
)

const (
	checkoutKey   = "test-key-checkout"
	storefrontKey = "test-key-storefront"
	adminKey      = "test-key-admin"
)

func newServer(t *testing.T) (*api.Server, *audit.Log) {
	t.Helper()
	keys := map[string]api.Identity{
		checkoutKey:   {Team: "checkout", Role: "team"},
		storefrontKey: {Team: "storefront", Role: "team"},
		adminKey:      {Team: "platform", Role: "admin"},
	}
	log, err := audit.Open("")
	if err != nil {
		t.Fatal(err)
	}
	return api.New(cluster.NewMemory(), keys, log), log
}

func do(t *testing.T, srv *api.Server, method, path, key, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if key != "" {
		req.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func mustStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("status = %d, want %d, body: %s", rec.Code, want, rec.Body.String())
	}
}

// setup creates namespace checkout-prod with one deployment, as checkout.
func setup(t *testing.T, srv *api.Server) {
	t.Helper()
	mustStatus(t, do(t, srv, "POST", "/namespaces", checkoutKey, `{"name":"checkout-prod"}`), http.StatusCreated)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments", checkoutKey,
		`{"name":"checkout-api","image":"checkout-api:1.4.0","replicas":2}`), http.StatusCreated)
}

func TestHealthOpenWithoutKey(t *testing.T) {
	srv, _ := newServer(t)
	rec := do(t, srv, "GET", "/health", "", "")
	mustStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("unexpected health body: %s", rec.Body.String())
	}
}

func TestReadsRejectedWithoutKey(t *testing.T) {
	srv, _ := newServer(t)
	mustStatus(t, do(t, srv, "GET", "/namespaces", "", ""), http.StatusUnauthorized)
}

func TestUnknownKeyRejected(t *testing.T) {
	srv, _ := newServer(t)
	mustStatus(t, do(t, srv, "GET", "/namespaces", "no-such-key", ""), http.StatusUnauthorized)
}

func TestCreateNamespaceAndDeploymentHappyPath(t *testing.T) {
	srv, _ := newServer(t)
	setup(t, srv)

	rec := do(t, srv, "GET", "/namespaces", checkoutKey, "")
	mustStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), "checkout-prod") {
		t.Fatalf("namespace missing from list: %s", rec.Body.String())
	}

	rec = do(t, srv, "GET", "/namespaces/checkout-prod/deployments", storefrontKey, "")
	mustStatus(t, rec, http.StatusOK) // reads are open to any valid key
	var out struct {
		Deployments []cluster.Deployment `json:"deployments"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Deployments) != 1 || out.Deployments[0].Image != "checkout-api:1.4.0" || out.Deployments[0].Replicas != 2 {
		t.Fatalf("unexpected deployments: %+v", out.Deployments)
	}
}

func TestTeamCannotCreateNamespaceOutsidePrefix(t *testing.T) {
	srv, _ := newServer(t)
	mustStatus(t, do(t, srv, "POST", "/namespaces", checkoutKey, `{"name":"storefront-prod"}`), http.StatusForbidden)
	// Admin can create any namespace.
	mustStatus(t, do(t, srv, "POST", "/namespaces", adminKey, `{"name":"storefront-prod"}`), http.StatusCreated)
}

func TestTeamCannotTouchAnotherTeamsDeployment(t *testing.T) {
	srv, _ := newServer(t)
	setup(t, srv)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments", storefrontKey,
		`{"name":"rogue","image":"rogue:1","replicas":1}`), http.StatusForbidden)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments/checkout-api/scale", storefrontKey,
		`{"replicas":0}`), http.StatusForbidden)
	mustStatus(t, do(t, srv, "DELETE", "/namespaces/checkout-prod/deployments/checkout-api", storefrontKey, ""), http.StatusForbidden)
}

func TestDryRunCreateChangesNothing(t *testing.T) {
	srv, _ := newServer(t)
	setup(t, srv)

	rec := do(t, srv, "POST", "/namespaces/checkout-prod/deployments?dry_run=true", checkoutKey,
		`{"name":"checkout-worker","image":"checkout-worker:0.9.0","replicas":1}`)
	mustStatus(t, rec, http.StatusOK)
	if !strings.Contains(rec.Body.String(), `"dry_run":true`) {
		t.Fatalf("expected dry_run report, got: %s", rec.Body.String())
	}

	rec = do(t, srv, "GET", "/namespaces/checkout-prod/deployments", checkoutKey, "")
	mustStatus(t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "checkout-worker") {
		t.Fatalf("dry run created a deployment: %s", rec.Body.String())
	}

	rec = do(t, srv, "POST", "/namespaces?dry_run=true", checkoutKey, `{"name":"checkout-staging"}`)
	mustStatus(t, rec, http.StatusOK)
	rec = do(t, srv, "GET", "/namespaces", checkoutKey, "")
	if strings.Contains(rec.Body.String(), "checkout-staging") {
		t.Fatalf("dry run created a namespace: %s", rec.Body.String())
	}
}

func TestScaleToZeroAndBack(t *testing.T) {
	srv, _ := newServer(t)
	setup(t, srv)
	replicas := func() int {
		rec := do(t, srv, "GET", "/namespaces/checkout-prod/deployments", checkoutKey, "")
		var out struct {
			Deployments []cluster.Deployment `json:"deployments"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out.Deployments[0].Replicas
	}
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments/checkout-api/scale", checkoutKey, `{"replicas":0}`), http.StatusOK)
	if got := replicas(); got != 0 {
		t.Fatalf("replicas after scale to zero = %d", got)
	}
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments/checkout-api/scale", checkoutKey, `{"replicas":3}`), http.StatusOK)
	if got := replicas(); got != 3 {
		t.Fatalf("replicas after scale up = %d", got)
	}
}

func TestDeleteDeploymentAndNamespace(t *testing.T) {
	srv, _ := newServer(t)
	setup(t, srv)
	mustStatus(t, do(t, srv, "DELETE", "/namespaces/checkout-prod/deployments/checkout-api", checkoutKey, ""), http.StatusOK)
	rec := do(t, srv, "GET", "/namespaces/checkout-prod/deployments", checkoutKey, "")
	if strings.Contains(rec.Body.String(), "checkout-api") {
		t.Fatalf("deployment still listed after delete: %s", rec.Body.String())
	}
	mustStatus(t, do(t, srv, "DELETE", "/namespaces/checkout-prod", checkoutKey, ""), http.StatusOK)
	rec = do(t, srv, "GET", "/namespaces", checkoutKey, "")
	if strings.Contains(rec.Body.String(), "checkout-prod") {
		t.Fatalf("namespace still listed after delete: %s", rec.Body.String())
	}
}

func TestConflictOnDuplicateCreate(t *testing.T) {
	srv, _ := newServer(t)
	setup(t, srv)
	mustStatus(t, do(t, srv, "POST", "/namespaces", checkoutKey, `{"name":"checkout-prod"}`), http.StatusConflict)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments", checkoutKey,
		`{"name":"checkout-api","image":"checkout-api:1.4.0","replicas":2}`), http.StatusConflict)
}

func TestNotFoundPaths(t *testing.T) {
	srv, _ := newServer(t)
	mustStatus(t, do(t, srv, "GET", "/namespaces/ghost/deployments", checkoutKey, ""), http.StatusNotFound)
	mustStatus(t, do(t, srv, "DELETE", "/namespaces/ghost", adminKey, ""), http.StatusNotFound)
	setup(t, srv)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments/ghost/scale", checkoutKey, `{"replicas":1}`), http.StatusNotFound)
	mustStatus(t, do(t, srv, "DELETE", "/namespaces/checkout-prod/deployments/ghost", checkoutKey, ""), http.StatusNotFound)
}

func TestInvalidBody(t *testing.T) {
	srv, _ := newServer(t)
	mustStatus(t, do(t, srv, "POST", "/namespaces", checkoutKey, `not json`), http.StatusBadRequest)
	mustStatus(t, do(t, srv, "POST", "/namespaces", checkoutKey, `{}`), http.StatusBadRequest)
	setup(t, srv)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments", checkoutKey,
		`{"name":"bad","image":"","replicas":1}`), http.StatusBadRequest)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments/checkout-api/scale", checkoutKey,
		`{"replicas":-2}`), http.StatusBadRequest)
	mustStatus(t, do(t, srv, "POST", "/namespaces/checkout-prod/deployments/checkout-api/scale", checkoutKey,
		`{}`), http.StatusBadRequest)
}

func TestAuditRecordsMutationsDeniedAndDryRun(t *testing.T) {
	srv, log := newServer(t)
	setup(t, srv) // 2 mutations by checkout
	mustStatus(t, do(t, srv, "POST", "/namespaces", storefrontKey, `{"name":"checkout-evil"}`), http.StatusForbidden)
	mustStatus(t, do(t, srv, "POST", "/namespaces?dry_run=true", checkoutKey, `{"name":"checkout-staging"}`), http.StatusOK)
	mustStatus(t, do(t, srv, "GET", "/namespaces", checkoutKey, ""), http.StatusOK) // reads are not audited

	entries := log.Entries()
	if len(entries) != 4 {
		t.Fatalf("audit entries = %d, want 4: %+v", len(entries), entries)
	}
	var denied, dry *audit.Entry
	for i := range entries {
		if entries[i].Result == "denied" {
			denied = &entries[i]
		}
		if entries[i].DryRun {
			dry = &entries[i]
		}
	}
	if denied == nil || denied.Actor != "storefront" {
		t.Fatalf("denied entry missing or wrong actor: %+v", entries)
	}
	if dry == nil || dry.Actor != "checkout" || dry.Result != "dry_run" {
		t.Fatalf("dry run entry missing or wrong: %+v", entries)
	}
	if entries[0].Actor != "checkout" || entries[0].Action != "create_namespace" {
		t.Fatalf("first entry wrong: %+v", entries[0])
	}
}
