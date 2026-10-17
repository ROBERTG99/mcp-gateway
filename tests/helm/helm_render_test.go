//go:build integration

// Package helm_test renders deploy/helm/mcp-gateway with helm (./bin/helm from make tools, or helm
// on PATH; the test image ships one) and checks what every install gets, whatever the values file.
package helm_test

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const (
	chart = "../../deploy/helm/mcp-gateway"
	// kubeVersion matches scripts/helm_check.sh (kubeconform schemas).
	kubeVersion = "1.31.0"
)

var valuesFiles = []string{"values.yaml", "values-dynamodb.yaml"}

func helmBin(t *testing.T) string {
	t.Helper()
	if p, err := filepath.Abs("../../bin/helm"); err == nil {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	p, err := exec.LookPath("helm")
	if err != nil {
		t.Fatal("helm not found: run make tools (installs ./bin/helm)")
	}
	return p
}

func render(t *testing.T, args ...string) ([]map[string]any, error) {
	t.Helper()
	cmd := exec.Command(helmBin(t), append([]string{"template", "gw", chart, "--namespace", "mcpgw", "--kube-version", kubeVersion}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, errors.New(stderr.String())
	}
	var docs []map[string]any
	dec := yaml.NewDecoder(bytes.NewReader(out))
	for {
		var d map[string]any
		if err := dec.Decode(&d); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode rendered YAML: %v", err)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs, nil
}

func renderValues(t *testing.T, file string, extra ...string) []map[string]any {
	t.Helper()
	docs, err := render(t, append([]string{"-f", filepath.Join(chart, file)}, extra...)...)
	if err != nil {
		t.Fatalf("helm template -f %s: %v", file, err)
	}
	return docs
}

func get(m any, path ...string) any {
	for _, p := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[p]
	}
	return m
}

func kinds(docs []map[string]any) map[string][]map[string]any {
	out := map[string][]map[string]any{}
	for _, d := range docs {
		k, _ := d["kind"].(string)
		out[k] = append(out[k], d)
	}
	return out
}

// podSpecs returns every pod template in the rendered chart (Deployment and the migrate Job).
func podSpecs(docs []map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, d := range docs {
		if spec, ok := get(d, "spec", "template", "spec").(map[string]any); ok {
			out[d["kind"].(string)+"/"+get(d, "metadata", "name").(string)] = spec
		}
	}
	return out
}

// TestHelmRenderSecurityContext checks the hardened pod and container settings in every pod the
// chart renders, with both values files: non-root UID 65532, RuntimeDefault seccomp, read-only root
// filesystem, no privilege escalation, all capabilities dropped, no service-account token, resource
// requests and limits, and an image pinned to a tag other than latest.
func TestHelmRenderSecurityContext(t *testing.T) {
	for _, file := range valuesFiles {
		t.Run(file, func(t *testing.T) {
			pods := podSpecs(renderValues(t, file))
			if len(pods) < 2 {
				t.Fatalf("want the gateway Deployment and the migrate Job, got %v", slices.Collect(maps.Keys(pods)))
			}
			for name, spec := range pods {
				if get(spec, "securityContext", "runAsNonRoot") != true {
					t.Errorf("%s: pod runAsNonRoot not true", name)
				}
				if get(spec, "securityContext", "runAsUser") != 65532 {
					t.Errorf("%s: pod runAsUser %v, want 65532", name, get(spec, "securityContext", "runAsUser"))
				}
				if get(spec, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
					t.Errorf("%s: seccompProfile not RuntimeDefault", name)
				}
				if get(spec, "automountServiceAccountToken") != false {
					t.Errorf("%s: automountServiceAccountToken not false", name)
				}
				containers, _ := spec["containers"].([]any)
				if len(containers) == 0 {
					t.Fatalf("%s: no containers", name)
				}
				for _, c := range containers {
					cname := name + "/" + get(c, "name").(string)
					sc := get(c, "securityContext")
					if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true ||
						get(sc, "privileged") != false {
						t.Errorf("%s: container securityContext %v", cname, sc)
					}
					if drop, _ := get(sc, "capabilities", "drop").([]any); !slices.Contains(drop, any("ALL")) {
						t.Errorf("%s: capabilities.drop %v, want ALL", cname, drop)
					}
					if get(c, "resources", "requests", "cpu") == nil || get(c, "resources", "requests", "memory") == nil ||
						get(c, "resources", "limits", "memory") == nil {
						t.Errorf("%s: resources %v, want cpu and memory requests and a memory limit", cname, get(c, "resources"))
					}
					img, _ := get(c, "image").(string)
					if !strings.Contains(img, ":") || strings.HasSuffix(img, ":latest") {
						t.Errorf("%s: image %q must be pinned", cname, img)
					}
				}
			}
		})
	}
}

// TestHelmRenderObjects checks the FR-13 object set and the store selection of each values file.
func TestHelmRenderObjects(t *testing.T) {
	for _, file := range valuesFiles {
		t.Run(file, func(t *testing.T) {
			docs := renderValues(t, file)
			k := kinds(docs)
			for _, want := range []string{"Deployment", "Service", "ConfigMap", "HorizontalPodAutoscaler", "PodDisruptionBudget",
				"NetworkPolicy", "ServiceAccount", "Job"} {
				if len(k[want]) == 0 {
					t.Errorf("no %s rendered", want)
				}
			}
			dep := k["Deployment"][0]
			env := map[string]string{}
			c := get(dep, "spec", "template", "spec", "containers").([]any)[0]
			for _, e := range get(c, "env").([]any) {
				env[get(e, "name").(string)], _ = get(e, "value").(string)
			}
			store := map[string]string{"values.yaml": "redis", "values-dynamodb.yaml": "dynamodb"}[file]
			if env["MCPGW_IDEMPOTENCY_STORE"] != store {
				t.Errorf("MCPGW_IDEMPOTENCY_STORE = %q, want %q", env["MCPGW_IDEMPOTENCY_STORE"], store)
			}
			if env["MCPGW_DEV_ISSUER_URL"] != "" || env["MCPGW_DEV_ISSUER_ENABLED"] == "true" {
				t.Error("the dev issuer must stay off in the chart")
			}
			if env["MCPGW_CONSOLE_SECURE_COOKIES"] != "true" {
				t.Error("console cookies must be Secure in the chart")
			}
			// The gateway gets only the secrets it reads, each by key; the schema owner's URL and the
			// role passwords stay with the migrate Job.
			if get(c, "envFrom") != nil {
				t.Error("gateway container uses envFrom: it would receive every key of the Secret")
			}
			fromSecret := map[string]bool{}
			for _, e := range get(c, "env").([]any) {
				n := get(e, "name").(string)
				if get(e, "valueFrom", "secretKeyRef", "name") == "mcp-gateway" {
					fromSecret[n] = true
				}
				if (strings.Contains(n, "SECRET") || strings.Contains(n, "PASSWORD") || strings.HasSuffix(n, "_URL") && strings.Contains(n, "DATABASE")) &&
					get(e, "value") != nil {
					t.Errorf("%s has a literal value; secrets come from a Secret", n)
				}
				if n == "MCPGW_MIGRATE_DATABASE_URL" || strings.HasSuffix(n, "_DB_PASSWORD") {
					t.Errorf("gateway container receives %s", n)
				}
			}
			for _, n := range []string{"MCPGW_DATABASE_URL", "MCPGW_REDIS_URL", "MCPGW_APPROVAL_WEBHOOK_SECRET"} {
				if !fromSecret[n] {
					t.Errorf("%s not taken from the Secret by key", n)
				}
			}
			if get(c, "readinessProbe", "httpGet", "path") != "/readyz" || get(c, "livenessProbe", "httpGet", "path") != "/healthz" {
				t.Error("probes must use /readyz and /healthz")
			}
			if get(k["HorizontalPodAutoscaler"][0], "spec", "minReplicas") != 2 {
				t.Error("HPA minReplicas must be 2 so the PDB allows a voluntary disruption")
			}
		})
	}
}

// TestHelmValuesSchemaRejects checks that values.schema.json rejects bad values at render time.
func TestHelmValuesSchemaRejects(t *testing.T) {
	for _, bad := range [][]string{
		{"--set", "replicaCount=two"},
		{"--set", "idempotency.store=memcached"},
		{"--set", "image.tag=latest"},
		{"--set", "existingSecret="},
		{"--set", "unknownKey=1"},
	} {
		if _, err := render(t, bad...); err == nil {
			t.Errorf("helm template %v: rendered, want a schema error", bad)
		}
	}
}
