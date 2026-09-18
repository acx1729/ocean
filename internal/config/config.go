// Package config loads the node configuration from environment variables.
//
// Every setting is an environment variable with a KB_ prefix (specification
// section 13). The same values can be rendered from kb.yaml or Helm values; the
// binary itself only reads the environment.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Role selects which services a process runs.
type Role string

const (
	RoleAll    Role = "all"
	RoleAPI    Role = "api"
	RoleSync   Role = "sync"
	RoleWorker Role = "worker"
)

// RunsAPI reports whether this process serves the API, the web app and MCP.
func (r Role) RunsAPI() bool { return r == RoleAll || r == RoleAPI }

// RunsSync reports whether this process hosts sync rooms.
func (r Role) RunsSync() bool { return r == RoleAll || r == RoleSync }

// RunsWorker reports whether this process runs background jobs.
func (r Role) RunsWorker() bool { return r == RoleAll || r == RoleWorker }

// S3 configures the object storage endpoint. Every object written there is
// ciphertext produced by the node, whatever the store's own settings are.
type S3 struct {
	Endpoint  string
	Bucket    string
	AccessKey string
	SecretKey string
	Region    string
	// PathStyle addressing is the default for SeaweedFS, Ceph RGW and MinIO-compatible stores.
	PathStyle bool
}

// Limits are the request and document limits from specification section 12,
// each overridable with a KB_LIMITS_* variable.
type Limits struct {
	BlockMarkdownBytes   int64
	BlocksPerPage        int
	PageDocBytes         int64
	BatchOps             int
	AssetBytes           int64
	PropertiesPerBlock   int
	BoundariesPerPage    int
	ImportArchiveBytes   int64
	RequestBodyBytes     int64
	UpdateBytes          int64
	OpenDocsPerConn      int
	PushesPerSecond      int
	SubscribersPerRoom   int
	QueryTimeout         time.Duration
	AdminQueryTimeout    time.Duration
	QueryConcurrency     int
	MCPResponseBytes     int64
	RateAccessPerMinute  int
	RateAgentPerMinute   int
	RateChallengePerMin  int
	SessionsPerUser      int
}

// Config is the complete node configuration.
type Config struct {
	Role              Role
	PublicURL         string
	SyncURL           string
	Listen            string
	BootstrapAdminDID string

	DatabaseURL        string
	ReplicaDatabaseURL string
	FGADatabaseURL     string
	RiverDatabaseURL   string
	NATSURL            string

	S3 S3
	// ObjectStoreDir selects the filesystem backend of the ObjectStore interface
	// (desktop profile and tests) instead of S3.
	ObjectStoreDir string

	NodeKeyFile       string
	NodeKeyPassphrase string
	NodeKeyKMS        string
	// OperatorToken authenticates AdminService calls. When empty a token derived
	// from the node key is written next to the node key file at first start.
	OperatorToken string

	EVMRPCURL string

	SessionTTL time.Duration
	RefreshTTL time.Duration

	Limits Limits

	OTELEndpoint string
	LogLevel     string
	LogFormat    string

	// Dev relaxes transport-security assumptions (plain HTTP cookies) for local
	// development. Never set in production.
	Dev bool
	// TrustProxy makes rate limiting and logging use X-Forwarded-For; enable it
	// only behind an ingress that sets the header.
	TrustProxy bool
	// WebApp serves the embedded web app at /app.
	WebApp bool
	// Migrate applies embedded migrations at startup (the first process to take
	// the migration lock migrates).
	Migrate bool
	// DataDir holds node-local state such as the sealed node key when
	// KB_NODE_KEY_FILE is not set.
	DataDir string
}

// DefaultLimits are the specification defaults.
func DefaultLimits() Limits {
	return Limits{
		BlockMarkdownBytes:  100 << 10,
		BlocksPerPage:       20000,
		PageDocBytes:        20 << 20,
		BatchOps:            500,
		AssetBytes:          250 << 20,
		PropertiesPerBlock:  200,
		BoundariesPerPage:   200,
		ImportArchiveBytes:  2 << 30,
		RequestBodyBytes:    8 << 20,
		UpdateBytes:         1 << 20,
		OpenDocsPerConn:     20,
		PushesPerSecond:     100,
		SubscribersPerRoom:  100,
		QueryTimeout:        2 * time.Second,
		AdminQueryTimeout:   5 * time.Second,
		QueryConcurrency:    8,
		MCPResponseBytes:    1 << 20,
		RateAccessPerMinute: 600,
		RateAgentPerMinute:  300,
		RateChallengePerMin: 30,
		SessionsPerUser:     20,
	}
}

// FromEnv loads the configuration from the process environment.
func FromEnv() (*Config, error) { return Load(os.Getenv) }

// Load builds a Config from the given lookup function and validates it.
// Validation errors are collected so an operator sees every problem at once.
func Load(getenv func(string) string) (*Config, error) {
	var errs []error
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }
	getDefault := func(key, def string) string {
		if v := get(key); v != "" {
			return v
		}
		return def
	}
	getBool := func(key string, def bool) bool {
		v := strings.ToLower(get(key))
		switch v {
		case "":
			return def
		case "1", "true", "yes", "on":
			return true
		case "0", "false", "no", "off":
			return false
		}
		errs = append(errs, fmt.Errorf("%s: invalid boolean %q", key, v))
		return def
	}
	getDuration := func(key string, def time.Duration) time.Duration {
		v := get(key)
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return d
	}
	getInt := func(key string, def int) int {
		v := get(key)
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("%s: invalid integer %q", key, v))
			return def
		}
		return n
	}
	getBytes := func(key string, def int64) int64 {
		v := get(key)
		if v == "" {
			return def
		}
		n, err := ParseBytes(v)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", key, err))
			return def
		}
		return n
	}

	c := &Config{
		Role:               Role(getDefault("KB_ROLE", string(RoleAll))),
		PublicURL:          strings.TrimRight(get("KB_PUBLIC_URL"), "/"),
		Listen:             getDefault("KB_LISTEN", ":8080"),
		BootstrapAdminDID:  get("KB_BOOTSTRAP_ADMIN_DID"),
		DatabaseURL:        get("KB_DATABASE_URL"),
		ReplicaDatabaseURL: get("KB_REPLICA_DATABASE_URL"),
		FGADatabaseURL:     get("KB_FGA_DATABASE_URL"),
		RiverDatabaseURL:   get("KB_RIVER_DATABASE_URL"),
		NATSURL:            get("KB_NATS_URL"),
		S3: S3{
			Endpoint:  get("KB_S3_ENDPOINT"),
			Bucket:    get("KB_S3_BUCKET"),
			AccessKey: get("KB_S3_ACCESS_KEY"),
			SecretKey: get("KB_S3_SECRET_KEY"),
			Region:    getDefault("KB_S3_REGION", "us-east-1"),
			PathStyle: getBool("KB_S3_PATH_STYLE", true),
		},
		ObjectStoreDir:    get("KB_OBJECT_STORE_DIR"),
		NodeKeyFile:       get("KB_NODE_KEY_FILE"),
		NodeKeyPassphrase: getenv("KB_NODE_KEY_PASSPHRASE"),
		NodeKeyKMS:        get("KB_NODE_KEY_KMS"),
		OperatorToken:     get("KB_OPERATOR_TOKEN"),
		EVMRPCURL:         get("KB_EVM_RPC_URL"),
		SessionTTL:        getDuration("KB_SESSION_TTL", 15*time.Minute),
		RefreshTTL:        getDuration("KB_REFRESH_TTL", 720*time.Hour),
		OTELEndpoint:      get("KB_OTEL_EXPORTER_OTLP_ENDPOINT"),
		LogLevel:          getDefault("KB_LOG_LEVEL", "info"),
		LogFormat:         getDefault("KB_LOG_FORMAT", "json"),
		Dev:               getBool("KB_DEV", false),
		TrustProxy:        getBool("KB_TRUST_PROXY", false),
		WebApp:            getBool("KB_WEB_APP", true),
		Migrate:           getBool("KB_MIGRATE", true),
		DataDir:           getDefault("KB_DATA_DIR", "/var/lib/kb"),
	}
	c.SyncURL = strings.TrimRight(getDefault("KB_SYNC_URL", c.PublicURL), "/")

	l := DefaultLimits()
	l.BlockMarkdownBytes = getBytes("KB_LIMITS_BLOCK_MARKDOWN_BYTES", l.BlockMarkdownBytes)
	l.BlocksPerPage = getInt("KB_LIMITS_BLOCKS_PER_PAGE", l.BlocksPerPage)
	l.PageDocBytes = getBytes("KB_LIMITS_PAGE_DOC_BYTES", l.PageDocBytes)
	l.BatchOps = getInt("KB_LIMITS_BATCH_OPS", l.BatchOps)
	l.AssetBytes = getBytes("KB_LIMITS_ASSET_BYTES", l.AssetBytes)
	l.PropertiesPerBlock = getInt("KB_LIMITS_PROPERTIES_PER_BLOCK", l.PropertiesPerBlock)
	l.BoundariesPerPage = getInt("KB_LIMITS_BOUNDARIES_PER_PAGE", l.BoundariesPerPage)
	l.ImportArchiveBytes = getBytes("KB_LIMITS_IMPORT_ARCHIVE_BYTES", l.ImportArchiveBytes)
	l.RequestBodyBytes = getBytes("KB_LIMITS_REQUEST_BODY_BYTES", l.RequestBodyBytes)
	l.UpdateBytes = getBytes("KB_LIMITS_UPDATE_BYTES", l.UpdateBytes)
	l.OpenDocsPerConn = getInt("KB_LIMITS_OPEN_DOCS_PER_CONN", l.OpenDocsPerConn)
	l.PushesPerSecond = getInt("KB_LIMITS_PUSHES_PER_SECOND", l.PushesPerSecond)
	l.SubscribersPerRoom = getInt("KB_LIMITS_SUBSCRIBERS_PER_ROOM", l.SubscribersPerRoom)
	l.QueryTimeout = getDuration("KB_LIMITS_QUERY_TIMEOUT", l.QueryTimeout)
	l.AdminQueryTimeout = getDuration("KB_LIMITS_ADMIN_QUERY_TIMEOUT", l.AdminQueryTimeout)
	l.QueryConcurrency = getInt("KB_LIMITS_QUERY_CONCURRENCY", l.QueryConcurrency)
	l.MCPResponseBytes = getBytes("KB_LIMITS_MCP_RESPONSE_BYTES", l.MCPResponseBytes)
	l.RateAccessPerMinute = getInt("KB_LIMITS_RATE_ACCESS_PER_MINUTE", l.RateAccessPerMinute)
	l.RateAgentPerMinute = getInt("KB_LIMITS_RATE_AGENT_PER_MINUTE", l.RateAgentPerMinute)
	l.RateChallengePerMin = getInt("KB_LIMITS_RATE_CHALLENGE_PER_MINUTE", l.RateChallengePerMin)
	l.SessionsPerUser = getInt("KB_LIMITS_SESSIONS_PER_USER", l.SessionsPerUser)
	c.Limits = l

	errs = append(errs, c.validate()...)
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return c, nil
}

func (c *Config) validate() []error {
	var errs []error
	switch c.Role {
	case RoleAll, RoleAPI, RoleSync, RoleWorker:
	default:
		errs = append(errs, fmt.Errorf("KB_ROLE: must be one of all, api, sync, worker (got %q)", c.Role))
	}
	if c.PublicURL == "" {
		errs = append(errs, errors.New("KB_PUBLIC_URL is required"))
	} else if u, err := url.Parse(c.PublicURL); err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		errs = append(errs, fmt.Errorf("KB_PUBLIC_URL: must be an absolute http(s) URL (got %q)", c.PublicURL))
	} else if u.Scheme == "http" && !c.Dev && !isLoopback(u.Hostname()) {
		errs = append(errs, errors.New("KB_PUBLIC_URL: plain http is only allowed for loopback hosts or with KB_DEV=true"))
	}
	for _, kv := range []struct{ k, v string }{
		{"KB_DATABASE_URL", c.DatabaseURL},
		{"KB_FGA_DATABASE_URL", c.FGADatabaseURL},
		{"KB_RIVER_DATABASE_URL", c.RiverDatabaseURL},
		{"KB_NATS_URL", c.NATSURL},
	} {
		if kv.v == "" {
			errs = append(errs, fmt.Errorf("%s is required", kv.k))
		}
	}
	if c.ObjectStoreDir == "" {
		for _, kv := range []struct{ k, v string }{
			{"KB_S3_ENDPOINT", c.S3.Endpoint},
			{"KB_S3_BUCKET", c.S3.Bucket},
			{"KB_S3_ACCESS_KEY", c.S3.AccessKey},
			{"KB_S3_SECRET_KEY", c.S3.SecretKey},
		} {
			if kv.v == "" {
				errs = append(errs, fmt.Errorf("%s is required unless KB_OBJECT_STORE_DIR selects the filesystem backend", kv.k))
			}
		}
	}
	if c.NodeKeyKMS == "" && c.NodeKeyPassphrase == "" {
		errs = append(errs, errors.New("node key custody: set KB_NODE_KEY_PASSPHRASE (sealed file) or KB_NODE_KEY_KMS"))
	}
	if c.NodeKeyKMS != "" && c.NodeKeyPassphrase != "" {
		errs = append(errs, errors.New("node key custody: KB_NODE_KEY_PASSPHRASE and KB_NODE_KEY_KMS are mutually exclusive"))
	}
	if c.SessionTTL < time.Minute || c.SessionTTL > 24*time.Hour {
		errs = append(errs, errors.New("KB_SESSION_TTL: must be between 1m and 24h"))
	}
	if c.RefreshTTL < time.Hour {
		errs = append(errs, errors.New("KB_REFRESH_TTL: must be at least 1h"))
	}
	switch c.LogFormat {
	case "json", "text":
	default:
		errs = append(errs, fmt.Errorf("KB_LOG_FORMAT: json or text (got %q)", c.LogFormat))
	}
	if c.Limits.BatchOps > 500 {
		errs = append(errs, errors.New("KB_LIMITS_BATCH_OPS: cannot exceed 500"))
	}
	return errs
}

// NodeKeyPath returns the sealed node key file path, defaulting into DataDir.
func (c *Config) NodeKeyPath() string {
	if c.NodeKeyFile != "" {
		return c.NodeKeyFile
	}
	return c.DataDir + "/node.key"
}

// PublicHost returns the host of the public URL (the SIWE domain).
func (c *Config) PublicHost() string {
	u, err := url.Parse(c.PublicURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// Secure reports whether cookies must carry the Secure flag.
func (c *Config) Secure() bool {
	return strings.HasPrefix(c.PublicURL, "https://")
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasSuffix(host, ".localhost")
}

// ParseBytes parses sizes such as "8MB", "250MiB", "2GB", "1024".
func ParseBytes(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	for _, suf := range []struct {
		s string
		m int64
	}{
		{"GIB", 1 << 30}, {"MIB", 1 << 20}, {"KIB", 1 << 10},
		{"GB", 1 << 30}, {"MB", 1 << 20}, {"KB", 1 << 10}, {"B", 1},
	} {
		if strings.HasSuffix(s, suf.s) {
			s = strings.TrimSpace(strings.TrimSuffix(s, suf.s))
			mult = suf.m
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return n * mult, nil
}
