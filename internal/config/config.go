package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all application configuration.
type Config struct {
	Server      ServerConfig
	Database    DatabaseConfig
	OIDC        OIDCConfig
	NATS        NATSConfig
	Vault       VaultConfig
	VCenter     VCenterConfig
	OPNsense    OPNsenseConfig
	ObjectStore ObjectStoreConfig
}

// ObjectStoreConfig holds the S3/MinIO settings used to stage browser-uploaded
// installer images before they are imported into vCenter. Uploads are handed to
// the browser as presigned URLs, so Endpoint must be the address the *browser*
// can reach (and, because Crucible is served over HTTPS, it must itself be
// HTTPS or the browser blocks the request as mixed content).
type ObjectStoreConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Prefix    string
	UseSSL    bool
}

type ServerConfig struct {
	Host           string
	Port           int
	AllowedOrigins []string
}

type DatabaseConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	DBName   string
	SSLMode  string
}

func (d DatabaseConfig) DSN() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%d/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.DBName, d.SSLMode,
	)
}

type OIDCConfig struct {
	IssuerURL             string
	ClientID              string
	ClientSecret          string
	RedirectURL           string
	Scopes                []string
	PostLogoutRedirectURI string
}

type NATSConfig struct {
	URL string
}

type VaultConfig struct {
	Address string
	Role    string
	Mount   string
}

// VCenterConfig holds vCenter connection settings.
type VCenterConfig struct {
	URL        string
	User       string
	Password   string
	Datacenter string
	Datastore  string
	// ISODatastore holds installer media (ISOs) and is deliberately separate
	// from Datastore, which holds VM disks. Conflating them uploads multi-GB
	// installer images onto the VM datastore. Note the trailing capital "S" in
	// the real name -- it is spelled NAS-BackupsAndISOS in vCenter.
	ISODatastore string
	// ISOFolder is the datastore-relative directory that uploaded ISOs land in.
	ISOFolder string
	VMFolder  string
	// TemplatesFolder is the full inventory path to the vCenter folder housing
	// golden-image VMs that admins can register as Crucible templates (e.g.,
	// "/JMAL-Datacenter/vm/Templates"). Used by the admin folder-enumeration
	// endpoint; not used by provisioning.
	TemplatesFolder string
	ResourcePools   []string
	Hosts           []string
	Insecure        bool

	// HealthPushgatewayURL enables the in-process vCenter credentials
	// health probe (see internal/vsphere/health). When set, the api-gateway
	// performs a fresh login every HealthCheckInterval and pushes
	// vsphere_login_success to this Pushgateway. Empty disables the probe.
	HealthPushgatewayURL string
	// HealthCheckInterval is how often the probe runs. Defaults to 5m.
	HealthCheckInterval time.Duration
}

// OPNsenseConfig holds OPNsense connection settings.
type OPNsenseConfig struct {
	BaseURL     string
	APIKey      string
	APISecret   string
	SSHHost     string
	SSHUser     string
	SSHPassword string
}

// Load reads configuration from environment variables.
// In production, these are injected by Vault sidecar or K8s secrets.
func Load() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Host:           getEnv("SERVER_HOST", "0.0.0.0"),
			Port:           getEnvInt("SERVER_PORT", 8080),
			AllowedOrigins: splitEnv("ALLOWED_ORIGINS", "https://crucible.jmal.io"),
		},
		Database: DatabaseConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnvInt("DB_PORT", 5432),
			User:     getEnv("DB_USER", "selfservice"),
			Password: getEnv("DB_PASSWORD", ""),
			DBName:   getEnv("DB_NAME", "selfservice"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		OIDC: OIDCConfig{
			IssuerURL:             getEnv("OIDC_ISSUER_URL", ""),
			ClientID:              getEnv("OIDC_CLIENT_ID", ""),
			ClientSecret:          getEnv("OIDC_CLIENT_SECRET", ""),
			RedirectURL:           getEnv("OIDC_REDIRECT_URL", ""),
			Scopes:                []string{"openid", "profile", "email", "groups"},
			PostLogoutRedirectURI: getEnv("OIDC_POST_LOGOUT_REDIRECT_URI", "https://crucible.jmal.io/login"),
		},
		NATS: NATSConfig{
			URL: getEnv("NATS_URL", "nats://localhost:4222"),
		},
		Vault: VaultConfig{
			Address: getEnv("VAULT_ADDR", ""),
			Role:    getEnv("VAULT_ROLE", "selfservice"),
			Mount:   getEnv("VAULT_MOUNT", "kubernetes"),
		},
		VCenter: VCenterConfig{
			URL:                  getEnv("VCENTER_URL", "https://vcenter.lab.jmal.io/sdk"),
			User:                 getEnv("VCENTER_USER", ""),
			Password:             getEnv("VCENTER_PASSWORD", ""),
			Datacenter:           getEnv("VCENTER_DATACENTER", "JMAL-Datacenter"),
			Datastore:            getEnv("VCENTER_DATASTORE", "NAS-vmstore"),
			ISODatastore:         getEnv("VCENTER_ISO_DATASTORE", "NAS-BackupsAndISOS"),
			ISOFolder:            getEnv("VCENTER_ISO_FOLDER", "ISOs"),
			VMFolder:             getEnv("VCENTER_VM_FOLDER", "Student-VMs"),
			TemplatesFolder:      getEnv("VCENTER_TEMPLATES_FOLDER", "/JMAL-Datacenter/vm/Templates"),
			ResourcePools:        splitEnv("VCENTER_RESOURCE_POOLS", "/JMAL-Datacenter/host/Intel-Cluster/Resources/Student-VMs,/JMAL-Datacenter/host/AMD-Cluster/Resources/Student-VMs"),
			Hosts:                splitEnv("VCENTER_HOSTS", "esxi1.lab.jmal.io,esxi2.lab.jmal.io,nuc1.lab.jmal.io,nuc2.lab.jmal.io,nuc3.lab.jmal.io"),
			Insecure:             getEnvBool("VCENTER_INSECURE", true),
			HealthPushgatewayURL: getEnv("VCENTER_HEALTH_PUSHGATEWAY_URL", ""),
			HealthCheckInterval:  getEnvDuration("VCENTER_HEALTH_INTERVAL", 5*time.Minute),
		},
		OPNsense: OPNsenseConfig{
			BaseURL:     getEnv("OPNSENSE_URL", "https://10.10.10.60/api"),
			APIKey:      getEnv("OPNSENSE_API_KEY", ""),
			APISecret:   getEnv("OPNSENSE_API_SECRET", ""),
			SSHHost:     getEnv("OPNSENSE_SSH_HOST", "10.10.10.60:22"),
			SSHUser:     getEnv("OPNSENSE_SSH_USER", "root"),
			SSHPassword: getEnv("OPNSENSE_SSH_PASSWORD", ""),
		},
		ObjectStore: ObjectStoreConfig{
			Endpoint:  getEnv("OBJECTSTORE_ENDPOINT", ""),
			AccessKey: getEnv("OBJECTSTORE_ACCESS_KEY", ""),
			SecretKey: getEnv("OBJECTSTORE_SECRET_KEY", ""),
			Bucket:    getEnv("OBJECTSTORE_BUCKET", "isos"),
			Prefix:    getEnv("OBJECTSTORE_PREFIX", "crucible"),
			UseSSL:    getEnvBool("OBJECTSTORE_USE_SSL", true),
		},
	}

	return cfg, nil
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		b, err := strconv.ParseBool(v)
		if err == nil {
			return b
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		d, err := time.ParseDuration(v)
		if err == nil {
			return d
		}
	}
	return fallback
}

func splitEnv(key, fallback string) []string {
	v := getEnv(key, fallback)
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}
