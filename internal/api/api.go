// Package api is the HTTP layer: authentication, tenancy policy,
// dry run handling, and audit logging over a cluster.Cluster backend.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"platform-api/internal/audit"
	"platform-api/internal/cluster"
)

type Identity struct {
	Team string `json:"team"`
	Role string `json:"role"` // "admin" or "team"
}

// LoadKeys reads the API key config file. The file maps each key to
// the team it belongs to and the role it carries.
func LoadKeys(path string) (map[string]Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]Identity
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse keys file: %w", err)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("keys file %s has no keys", path)
	}
	for key, id := range raw {
		if id.Team == "" || (id.Role != "admin" && id.Role != "team") {
			return nil, fmt.Errorf("key %q: team is required and role must be admin or team", key)
		}
	}
	return raw, nil
}

type Server struct {
	backend cluster.Cluster
	keys    map[string]Identity
	audit   *audit.Log
	mux     *http.ServeMux
}

func New(backend cluster.Cluster, keys map[string]Identity, log *audit.Log) *Server {
	s := &Server{backend: backend, keys: keys, audit: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.HandleFunc("GET /namespaces", s.auth(s.listNamespaces))
	mux.HandleFunc("POST /namespaces", s.auth(s.createNamespace))
	mux.HandleFunc("DELETE /namespaces/{name}", s.auth(s.deleteNamespace))
	mux.HandleFunc("GET /namespaces/{ns}/deployments", s.auth(s.listDeployments))
	mux.HandleFunc("POST /namespaces/{ns}/deployments", s.auth(s.createDeployment))
	mux.HandleFunc("POST /namespaces/{ns}/deployments/{name}/scale", s.auth(s.scaleDeployment))
	mux.HandleFunc("DELETE /namespaces/{ns}/deployments/{name}", s.auth(s.deleteDeployment))
	s.mux = mux
	return s
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

type ctxKey string

const identityKey ctxKey = "identity"

func (s *Server) auth(next func(http.ResponseWriter, *http.Request, Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-API-Key")
		id, ok := s.keys[key]
		if key == "" || !ok {
			writeError(w, http.StatusUnauthorized, "missing or unknown API key")
			return
		}
		next(w, r, id)
	}
}

// canMutate enforces prefix tenancy: a team identity may only mutate
// namespaces whose names start with its team name plus a dash. Admins
// may mutate anything.
func canMutate(id Identity, namespace string) bool {
	if id.Role == "admin" {
		return true
	}
	return strings.HasPrefix(namespace, id.Team+"-")
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func statusFor(err error) int {
	switch {
	case errors.Is(err, cluster.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, cluster.ErrConflict):
		return http.StatusConflict
	case errors.Is(err, cluster.ErrInvalid):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func dryRun(r *http.Request) bool {
	return r.URL.Query().Get("dry_run") == "true"
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) listNamespaces(w http.ResponseWriter, r *http.Request, id Identity) {
	writeJSON(w, http.StatusOK, map[string]any{"namespaces": s.backend.ListNamespaces()})
}

func (s *Server) createNamespace(w http.ResponseWriter, r *http.Request, id Identity) {
	dry := dryRun(r)
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		s.audit.Record(id.Team, "create_namespace", body.Name, dry, "invalid")
		writeError(w, http.StatusBadRequest, "body must be JSON with a name field")
		return
	}
	if !canMutate(id, body.Name) {
		s.audit.Record(id.Team, "create_namespace", body.Name, dry, "denied")
		writeError(w, http.StatusForbidden, "team "+id.Team+" can only manage namespaces prefixed with "+id.Team+"-")
		return
	}
	if dry {
		// Validate against current state without changing it.
		for _, ns := range s.backend.ListNamespaces() {
			if ns == body.Name {
				s.audit.Record(id.Team, "create_namespace", body.Name, dry, "conflict")
				writeError(w, http.StatusConflict, "namespace "+body.Name+" already exists")
				return
			}
		}
		s.audit.Record(id.Team, "create_namespace", body.Name, dry, "dry_run")
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "would_create": "namespace " + body.Name})
		return
	}
	if err := s.backend.CreateNamespace(body.Name); err != nil {
		s.audit.Record(id.Team, "create_namespace", body.Name, dry, "error")
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.audit.Record(id.Team, "create_namespace", body.Name, dry, "created")
	writeJSON(w, http.StatusCreated, map[string]string{"namespace": body.Name})
}

func (s *Server) deleteNamespace(w http.ResponseWriter, r *http.Request, id Identity) {
	dry := dryRun(r)
	name := r.PathValue("name")
	target := "namespace " + name
	if !canMutate(id, name) {
		s.audit.Record(id.Team, "delete_namespace", target, dry, "denied")
		writeError(w, http.StatusForbidden, "team "+id.Team+" can only manage namespaces prefixed with "+id.Team+"-")
		return
	}
	exists := false
	for _, ns := range s.backend.ListNamespaces() {
		if ns == name {
			exists = true
		}
	}
	if !exists {
		s.audit.Record(id.Team, "delete_namespace", target, dry, "not_found")
		writeError(w, http.StatusNotFound, "namespace "+name+" not found")
		return
	}
	if dry {
		s.audit.Record(id.Team, "delete_namespace", target, dry, "dry_run")
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "would_delete": target})
		return
	}
	if err := s.backend.DeleteNamespace(name); err != nil {
		s.audit.Record(id.Team, "delete_namespace", target, dry, "error")
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.audit.Record(id.Team, "delete_namespace", target, dry, "deleted")
	writeJSON(w, http.StatusOK, map[string]string{"deleted": target})
}

func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request, id Identity) {
	ns := r.PathValue("ns")
	deps, err := s.backend.ListDeployments(ns)
	if err != nil {
		writeError(w, statusFor(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deployments": deps})
}

func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request, id Identity) {
	dry := dryRun(r)
	ns := r.PathValue("ns")
	var body struct {
		Name     string `json:"name"`
		Image    string `json:"image"`
		Replicas int    `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
		s.audit.Record(id.Team, "create_deployment", ns+"/", dry, "invalid")
		writeError(w, http.StatusBadRequest, "body must be JSON with name, image, and replicas")
		return
	}
	target := ns + "/" + body.Name
	if !canMutate(id, ns) {
		s.audit.Record(id.Team, "create_deployment", target, dry, "denied")
		writeError(w, http.StatusForbidden, "team "+id.Team+" can only manage namespaces prefixed with "+id.Team+"-")
		return
	}
	d := cluster.Deployment{Name: body.Name, Namespace: ns, Image: body.Image, Replicas: body.Replicas}
	if dry {
		if _, err := s.backend.ListDeployments(ns); err != nil {
			s.audit.Record(id.Team, "create_deployment", target, dry, "not_found")
			writeError(w, statusFor(err), err.Error())
			return
		}
		existing, _ := s.backend.ListDeployments(ns)
		for _, e := range existing {
			if e.Name == d.Name {
				s.audit.Record(id.Team, "create_deployment", target, dry, "conflict")
				writeError(w, http.StatusConflict, "deployment "+target+" already exists")
				return
			}
		}
		if d.Image == "" || d.Replicas < 0 || d.Replicas > 50 {
			s.audit.Record(id.Team, "create_deployment", target, dry, "invalid")
			writeError(w, http.StatusBadRequest, "image is required and replicas must be between 0 and 50")
			return
		}
		s.audit.Record(id.Team, "create_deployment", target, dry, "dry_run")
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "would_create": d})
		return
	}
	if err := s.backend.CreateDeployment(d); err != nil {
		s.audit.Record(id.Team, "create_deployment", target, dry, "error")
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.audit.Record(id.Team, "create_deployment", target, dry, "created")
	writeJSON(w, http.StatusCreated, d)
}

func (s *Server) scaleDeployment(w http.ResponseWriter, r *http.Request, id Identity) {
	dry := dryRun(r)
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	target := ns + "/" + name
	var body struct {
		Replicas *int `json:"replicas"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Replicas == nil {
		s.audit.Record(id.Team, "scale_deployment", target, dry, "invalid")
		writeError(w, http.StatusBadRequest, "body must be JSON with a replicas field")
		return
	}
	if !canMutate(id, ns) {
		s.audit.Record(id.Team, "scale_deployment", target, dry, "denied")
		writeError(w, http.StatusForbidden, "team "+id.Team+" can only manage namespaces prefixed with "+id.Team+"-")
		return
	}
	if dry {
		deps, err := s.backend.ListDeployments(ns)
		if err != nil {
			s.audit.Record(id.Team, "scale_deployment", target, dry, "not_found")
			writeError(w, statusFor(err), err.Error())
			return
		}
		found := false
		for _, d := range deps {
			if d.Name == name {
				found = true
			}
		}
		if !found {
			s.audit.Record(id.Team, "scale_deployment", target, dry, "not_found")
			writeError(w, http.StatusNotFound, "deployment "+target+" not found")
			return
		}
		if *body.Replicas < 0 || *body.Replicas > 50 {
			s.audit.Record(id.Team, "scale_deployment", target, dry, "invalid")
			writeError(w, http.StatusBadRequest, "replicas must be between 0 and 50")
			return
		}
		s.audit.Record(id.Team, "scale_deployment", target, dry, "dry_run")
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "would_scale": target, "replicas": *body.Replicas})
		return
	}
	d, err := s.backend.ScaleDeployment(ns, name, *body.Replicas)
	if err != nil {
		s.audit.Record(id.Team, "scale_deployment", target, dry, "error")
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.audit.Record(id.Team, "scale_deployment", target, dry, "scaled")
	writeJSON(w, http.StatusOK, d)
}

func (s *Server) deleteDeployment(w http.ResponseWriter, r *http.Request, id Identity) {
	dry := dryRun(r)
	ns := r.PathValue("ns")
	name := r.PathValue("name")
	target := ns + "/" + name
	if !canMutate(id, ns) {
		s.audit.Record(id.Team, "delete_deployment", target, dry, "denied")
		writeError(w, http.StatusForbidden, "team "+id.Team+" can only manage namespaces prefixed with "+id.Team+"-")
		return
	}
	if dry {
		deps, err := s.backend.ListDeployments(ns)
		if err != nil {
			s.audit.Record(id.Team, "delete_deployment", target, dry, "not_found")
			writeError(w, statusFor(err), err.Error())
			return
		}
		found := false
		for _, d := range deps {
			if d.Name == name {
				found = true
			}
		}
		if !found {
			s.audit.Record(id.Team, "delete_deployment", target, dry, "not_found")
			writeError(w, http.StatusNotFound, "deployment "+target+" not found")
			return
		}
		s.audit.Record(id.Team, "delete_deployment", target, dry, "dry_run")
		writeJSON(w, http.StatusOK, map[string]any{"dry_run": true, "would_delete": target})
		return
	}
	if err := s.backend.DeleteDeployment(ns, name); err != nil {
		s.audit.Record(id.Team, "delete_deployment", target, dry, "error")
		writeError(w, statusFor(err), err.Error())
		return
	}
	s.audit.Record(id.Team, "delete_deployment", target, dry, "deleted")
	writeJSON(w, http.StatusOK, map[string]string{"deleted": target})
}
