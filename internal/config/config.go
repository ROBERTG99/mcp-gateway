// Package config loads the gateway configuration from MCPGW_ environment variables and the
// YAML files in config/. Every variable is declared once, in the Config struct tags, which is
// also the source of the documentation in .env.example (see Vars).
package config

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ROBERTG99/mcp-gateway/internal/catalog"
)

// Config is the gateway configuration. Tags: env (variable), default, desc, and optional oneof.
type Config struct {
	MCPAddr     string `env:"MCPGW_MCP_ADDR" default:":8210" desc:"MCP Streamable HTTP listen address"`
	AdminAddr   string `env:"MCPGW_ADMIN_ADDR" default:":8200" desc:"Admin API and console listen address"`
	MetricsAddr string `env:"MCPGW_METRICS_ADDR" default:":8211" desc:"Prometheus metrics listen address"`
	LogLevel    string `env:"MCPGW_LOG_LEVEL" default:"info" oneof:"debug,info,warn,error" desc:"Log level"`

	UpstreamsFile     string        `env:"MCPGW_UPSTREAMS_FILE" default:"config/upstreams.yaml" desc:"Upstream MCP servers (YAML)"`
	TenantsFile       string        `env:"MCPGW_TENANTS_FILE" default:"config/tenants.yaml" desc:"Per-tenant rate limits and daily budgets (YAML)"`
	PolicyDir         string        `env:"MCPGW_POLICY_DIR" default:"policies" desc:"Rego policy bundle directory"`
	PolicyReload      time.Duration `env:"MCPGW_POLICY_RELOAD_INTERVAL" default:"5s" desc:"How often the policy directory is checked for changes (0 disables)"`
	CatalogRefresh    time.Duration `env:"MCPGW_CATALOG_REFRESH" default:"30s" desc:"Upstream tool catalog refresh interval"`
	UpstreamTimeout   time.Duration `env:"MCPGW_UPSTREAM_TIMEOUT" default:"10s" desc:"Timeout of one upstream call"`
	PrincipalCacheTTL time.Duration `env:"MCPGW_PRINCIPAL_CACHE_TTL" default:"60s" desc:"Cache lifetime of a per-principal MCP server"`

	JWKSURL        string        `env:"MCPGW_JWKS_URL" default:"http://localhost:8212/.well-known/jwks.json" desc:"JWKS URL of the token issuer"`
	JWTIssuer      string        `env:"MCPGW_JWT_ISSUER" default:"mcp-gateway dev-issuer" desc:"Expected iss claim"`
	JWTAudience    string        `env:"MCPGW_JWT_AUDIENCE" default:"mcp-gateway" desc:"Expected aud claim"`
	JWKSCacheTTL   time.Duration `env:"MCPGW_JWKS_CACHE_TTL" default:"5m" desc:"JWKS cache lifetime"`
	JWKSStaleLimit time.Duration `env:"MCPGW_JWKS_STALE_LIMIT" default:"1h" desc:"How long cached keys stay usable while the issuer is unreachable"`

	DatabaseURL        string `env:"MCPGW_DATABASE_URL" default:"postgres://mcpgw_app:mcpgw_app@localhost:8220/mcpgw?sslmode=disable" desc:"Postgres URL for the gateway role (mcpgw_app)"`
	MigrateDatabaseURL string `env:"MCPGW_MIGRATE_DATABASE_URL" default:"postgres://postgres:postgres@localhost:8220/mcpgw?sslmode=disable" desc:"Postgres URL of the schema owner, used by migrate"`
	RedisURL           string `env:"MCPGW_REDIS_URL" default:"redis://localhost:8230/0" desc:"Redis URL (idempotency, rate limits, budgets)"`

	IdempotencyStore   string        `env:"MCPGW_IDEMPOTENCY_STORE" default:"redis" oneof:"redis,dynamodb,memory" desc:"Idempotency store backend"`
	IdempotencyTTL     time.Duration `env:"MCPGW_IDEMPOTENCY_TTL" default:"24h" desc:"How long completed idempotency records are kept"`
	IdempotencyLockTTL time.Duration `env:"MCPGW_IDEMPOTENCY_LOCK_TTL" default:"30s" desc:"Lifetime of an in-progress idempotency lock"`
	DynamoDBEndpoint   string        `env:"MCPGW_DYNAMODB_ENDPOINT" default:"" desc:"DynamoDB endpoint override (dynamodb-local: http://localhost:8240)"`
	DynamoDBTable      string        `env:"MCPGW_DYNAMODB_TABLE" default:"mcpgw_idempotency" desc:"DynamoDB idempotency table"`
	DynamoDBRegion     string        `env:"MCPGW_DYNAMODB_REGION" default:"us-east-1" desc:"DynamoDB region"`

	ApprovalTTL           time.Duration `env:"MCPGW_APPROVAL_TTL" default:"15m" desc:"Lifetime of a pending approval"`
	ApprovalWebhookURL    string        `env:"MCPGW_APPROVAL_WEBHOOK_URL" default:"" desc:"Approval webhook endpoint (empty disables delivery)"`
	ApprovalWebhookSecret string        `env:"MCPGW_APPROVAL_WEBHOOK_SECRET" default:"" secret:"true" desc:"HMAC-SHA256 key for X-MCPGW-Signature"`

	InjectionThreshold float64 `env:"MCPGW_INJECTION_THRESHOLD" default:"0.5" desc:"Injection score at or above which output is flagged (0..1)"`
	InjectionMode      string  `env:"MCPGW_INJECTION_MODE" default:"mark" oneof:"mark,block" desc:"mark: flag only; block: withhold flagged text"`

	AuditBuffer         int           `env:"MCPGW_AUDIT_BUFFER" default:"10000" desc:"Audit queue capacity (events)"`
	AuditFlushInterval  time.Duration `env:"MCPGW_AUDIT_FLUSH_INTERVAL" default:"50ms" desc:"Audit flush interval"`
	AuditBatch          int           `env:"MCPGW_AUDIT_BATCH" default:"500" desc:"Audit flush batch size"`
	AuditEnqueueTimeout time.Duration `env:"MCPGW_AUDIT_ENQUEUE_TIMEOUT" default:"1ms" desc:"Maximum wait to enqueue an audit event before -32048"`
	ShutdownTimeout     time.Duration `env:"MCPGW_SHUTDOWN_TIMEOUT" default:"10s" desc:"Graceful shutdown budget (audit drain included)"`
	RetentionInterval   time.Duration `env:"MCPGW_RETENTION_INTERVAL" default:"1h" desc:"How often retention deletes old approvals, findings and delivered outbox rows"`

	MaxBodyBytes         int64 `env:"MCPGW_MAX_BODY_BYTES" default:"1048576" desc:"Maximum request body (bytes)"`
	MaxResultBytes       int64 `env:"MCPGW_MAX_RESULT_BYTES" default:"4194304" desc:"Maximum upstream result (bytes)"`
	MaxStoredResultBytes int64 `env:"MCPGW_MAX_STORED_RESULT_BYTES" default:"262144" desc:"Maximum result stored for idempotent replay (bytes)"`
	MaxArgsDepth         int   `env:"MCPGW_MAX_ARGS_DEPTH" default:"32" desc:"Maximum nesting depth of tool arguments"`

	OTLPEndpoint string `env:"MCPGW_OTLP_ENDPOINT" default:"" desc:"OTLP/HTTP traces endpoint (empty disables export)"`

	ConsoleSessionKey    string `env:"MCPGW_CONSOLE_SESSION_KEY" default:"" secret:"true" desc:"HMAC key for console session cookies (random per process when empty)"`
	ConsoleSecureCookies bool   `env:"MCPGW_CONSOLE_SECURE_COOKIES" default:"false" desc:"Set the Secure flag on console cookies (enable behind TLS)"`
	DevIssuerURL         string `env:"MCPGW_DEV_ISSUER_URL" default:"http://localhost:8212" desc:"Dev issuer used by the console sign-in personas (development only)"`

	DevIssuerEnabled bool   `env:"MCPGW_DEV_ISSUER_ENABLED" default:"false" desc:"Allow the dev-issuer command to start (never in production)"`
	DevIssuerAddr    string `env:"MCPGW_DEV_ISSUER_ADDR" default:":8212" desc:"Dev issuer listen address"`
	DevIssuerKeyFile string `env:"MCPGW_DEV_ISSUER_KEY_FILE" default:"" desc:"PEM RSA key for the dev issuer (generated at start when empty)"`
}

// Var documents one environment variable.
type Var struct {
	Name, Default, Description string
	Secret                     bool
}

// Vars lists every variable declared by Config, in declaration order.
func Vars() []Var {
	t := reflect.TypeFor[Config]()
	out := make([]Var, 0, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		out = append(out, Var{Name: f.Tag.Get("env"), Default: f.Tag.Get("default"),
			Description: f.Tag.Get("desc"), Secret: f.Tag.Get("secret") == "true"})
	}
	return out
}

// Load builds a Config from defaults overridden by getenv (use os.Getenv in production).
func Load(getenv func(string) string) (Config, error) {
	var cfg Config
	v := reflect.ValueOf(&cfg).Elem()
	t := v.Type()
	var errs []error
	for i := range t.NumField() {
		f := t.Field(i)
		name := f.Tag.Get("env")
		raw := getenv(name)
		if raw == "" {
			raw = f.Tag.Get("default")
		}
		if err := set(v.Field(i), raw); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", name, raw, err))
			continue
		}
		if oneof := f.Tag.Get("oneof"); oneof != "" && !contains(oneof, raw) {
			errs = append(errs, fmt.Errorf("%s=%q: must be one of %s", name, raw, oneof))
		}
	}
	if cfg.InjectionThreshold < 0 || cfg.InjectionThreshold > 1 {
		errs = append(errs, fmt.Errorf("MCPGW_INJECTION_THRESHOLD=%v: must be within [0, 1]", cfg.InjectionThreshold))
	}
	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("config: %w", err)
	}
	return cfg, nil
}

func contains(list, s string) bool {
	start := 0
	for i := 0; i <= len(list); i++ {
		if i == len(list) || list[i] == ',' {
			if list[start:i] == s {
				return true
			}
			start = i + 1
		}
	}
	return false
}

func set(field reflect.Value, raw string) error {
	switch field.Interface().(type) {
	case string:
		field.SetString(raw)
	case time.Duration:
		d, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("invalid duration: %w", err)
		}
		field.SetInt(int64(d))
	case int, int64:
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid integer: %w", err)
		}
		field.SetInt(n)
	case float64:
		x, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return fmt.Errorf("invalid number: %w", err)
		}
		field.SetFloat(x)
	case bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return fmt.Errorf("invalid boolean: %w", err)
		}
		field.SetBool(b)
	default:
		return fmt.Errorf("unsupported field type %s", field.Type())
	}
	return nil
}

// LoadUpstreams reads and validates the upstreams file.
func LoadUpstreams(path string) ([]catalog.UpstreamConfig, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-provided configuration path
	if err != nil {
		return nil, fmt.Errorf("read upstreams: %w", err)
	}
	var file struct {
		Upstreams []catalog.UpstreamConfig `yaml:"upstreams"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	seen := map[string]bool{}
	for _, u := range file.Upstreams {
		if !catalog.ServerNamePattern.MatchString(u.Name) {
			return nil, fmt.Errorf("%s: invalid server name %q (want %s)", path, u.Name, catalog.ServerNamePattern)
		}
		if seen[u.Name] {
			return nil, fmt.Errorf("%s: duplicate server name %q", path, u.Name)
		}
		seen[u.Name] = true
		if u.URL == "" {
			return nil, fmt.Errorf("%s: server name %q has no url", path, u.Name)
		}
	}
	return file.Upstreams, nil
}

// Tenant holds the rate limits and daily budget of one tenant.
type Tenant struct {
	RatePerSecond      float64 `yaml:"rate_per_second"`
	Burst              int     `yaml:"burst"`
	AgentRatePerSecond float64 `yaml:"agent_rate_per_second"`
	AgentBurst         int     `yaml:"agent_burst"`
	DailyBudget        int64   `yaml:"daily_budget"`
}

// LoadTenants reads the tenants file.
func LoadTenants(path string) (map[string]Tenant, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-provided configuration path
	if err != nil {
		return nil, fmt.Errorf("read tenants: %w", err)
	}
	var file struct {
		Tenants map[string]Tenant `yaml:"tenants"`
	}
	if err := yaml.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	for id, t := range file.Tenants {
		if t.RatePerSecond <= 0 || t.Burst <= 0 || t.DailyBudget <= 0 {
			return nil, fmt.Errorf("%s: tenant %q needs positive rate_per_second, burst and daily_budget", path, id)
		}
	}
	return file.Tenants, nil
}
