// Package cluster defines the backend the API talks to. The HTTP layer
// only sees this interface, so the in-memory implementation used today
// can be swapped for a real Kubernetes backend later.
package cluster

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("already exists")
	ErrInvalid  = errors.New("invalid input")
)

type Deployment struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Image     string `json:"image"`
	Replicas  int    `json:"replicas"`
}

type Cluster interface {
	ListNamespaces() []string
	CreateNamespace(name string) error
	DeleteNamespace(name string) error
	ListDeployments(namespace string) ([]Deployment, error)
	CreateDeployment(d Deployment) error
	ScaleDeployment(namespace, name string, replicas int) (Deployment, error)
	DeleteDeployment(namespace, name string) error
}

// Memory is an in-memory Cluster. State lives for the life of the
// process and is safe for concurrent use.
type Memory struct {
	mu          sync.Mutex
	namespaces  map[string]bool
	deployments map[string]map[string]Deployment // namespace -> name -> deployment
}

func NewMemory() *Memory {
	return &Memory{
		namespaces:  make(map[string]bool),
		deployments: make(map[string]map[string]Deployment),
	}
}

func validName(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return !strings.HasPrefix(s, "-") && !strings.HasSuffix(s, "-")
}

func (m *Memory) ListNamespaces() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.namespaces))
	for ns := range m.namespaces {
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

func (m *Memory) CreateNamespace(name string) error {
	if !validName(name) {
		return fmt.Errorf("%w: namespace name %q", ErrInvalid, name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namespaces[name] {
		return fmt.Errorf("%w: namespace %s", ErrConflict, name)
	}
	m.namespaces[name] = true
	m.deployments[name] = make(map[string]Deployment)
	return nil
}

func (m *Memory) DeleteNamespace(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.namespaces[name] {
		return fmt.Errorf("%w: namespace %s", ErrNotFound, name)
	}
	delete(m.namespaces, name)
	delete(m.deployments, name)
	return nil
}

func (m *Memory) ListDeployments(namespace string) ([]Deployment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.namespaces[namespace] {
		return nil, fmt.Errorf("%w: namespace %s", ErrNotFound, namespace)
	}
	out := make([]Deployment, 0, len(m.deployments[namespace]))
	for _, d := range m.deployments[namespace] {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) CreateDeployment(d Deployment) error {
	if !validName(d.Name) {
		return fmt.Errorf("%w: deployment name %q", ErrInvalid, d.Name)
	}
	if d.Image == "" {
		return fmt.Errorf("%w: image is required", ErrInvalid)
	}
	if d.Replicas < 0 || d.Replicas > 50 {
		return fmt.Errorf("%w: replicas must be between 0 and 50", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.namespaces[d.Namespace] {
		return fmt.Errorf("%w: namespace %s", ErrNotFound, d.Namespace)
	}
	if _, ok := m.deployments[d.Namespace][d.Name]; ok {
		return fmt.Errorf("%w: deployment %s/%s", ErrConflict, d.Namespace, d.Name)
	}
	m.deployments[d.Namespace][d.Name] = d
	return nil
}

func (m *Memory) ScaleDeployment(namespace, name string, replicas int) (Deployment, error) {
	if replicas < 0 || replicas > 50 {
		return Deployment{}, fmt.Errorf("%w: replicas must be between 0 and 50", ErrInvalid)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.namespaces[namespace] {
		return Deployment{}, fmt.Errorf("%w: namespace %s", ErrNotFound, namespace)
	}
	d, ok := m.deployments[namespace][name]
	if !ok {
		return Deployment{}, fmt.Errorf("%w: deployment %s/%s", ErrNotFound, namespace, name)
	}
	d.Replicas = replicas
	m.deployments[namespace][name] = d
	return d, nil
}

func (m *Memory) DeleteDeployment(namespace, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.namespaces[namespace] {
		return fmt.Errorf("%w: namespace %s", ErrNotFound, namespace)
	}
	if _, ok := m.deployments[namespace][name]; !ok {
		return fmt.Errorf("%w: deployment %s/%s", ErrNotFound, namespace, name)
	}
	delete(m.deployments[namespace], name)
	return nil
}
