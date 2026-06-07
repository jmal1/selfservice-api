package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config holds all application configuration.
type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	OIDC     OIDCConfig
	NATS     NATSConfig
	Vault    VaultConfig
	VCenter  VCenterConfig
	OPNsense OPNsenseConfig
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
	IssuerURL    string
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Scopes       []string
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
	URL           string
	User          string
	Password      string
	Datacenter    string
	Datastore     string
	VMFolder      string
	// TemplatesFolder is the full inventory path to the vCenter folder housing
	// golden-image VMs that admins can register as Crucible templates (e.g.,
	// "/JMAL-Datacenter/vm/Templates"). Used by the admin folder-enumeration
	// endpoint; not used by provisioning.
	TemplatesFolder string
	ResourcePools   []string
	Hosts         []string
	Insecure      bool
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
			IssuerURL:    getEnv("OIDC_ISSUER_URL", ""),
			ClientID:     getEnv("OIDC_CLIENT_ID", ""),
			ClientSecret: getEnv("OIDC_CLIENT_SECRET", ""),
			RedirectURL:  getEnv("OIDC_REDIRECT_URL", ""),
			Scopes:       []string{"openid", "profile", "email", "groups"},
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
			URL:           getEnv("VCENTER_URL", "https://vcenter.lab.jmal.io/sdk"),
			User:          getEnv("VCENTER_USER", ""),
			Password:      getEnv("VCENTER_PASSWORD", ""),
			Datacenter:    getEnv("VCENTER_DATACENTER", "JMAL-Datacenter"),
			Datastore:     getEnv("VCENTER_DATASTORE", "NAS-vmstore"),
			VMFolder:        getEnv("VCENTER_VM_FOLDER", "Student-VMs"),
			TemplatesFolder: getEnv("VCENTER_TEMPLATES_FOLDER", "/JMAL-Datacenter/vm/Templates"),
			ResourcePools:   splitEnv("VCENTER_RESOURCE_POOLS", "/JMAL-Datacenter/host/Intel-Cluster/Resources/Student-VMs,/JMAL-Datacenter/host/AMD-Cluster/Resources/Student-VMs"),
			Hosts:         splitEnv("VCENTER_HOSTS", "esxi1.lab.jmal.io,esxi2.lab.jmal.io,nuc1.lab.jmal.io,nuc2.lab.jmal.io,nuc3.lab.jmal.io"),
			Insecure:      getEnvBool("VCENTER_INSECURE", true),
		},
		OPNsense: OPNsenseConfig{
			BaseURL:     getEnv("OPNSENSE_URL", "https://10.10.10.60/api"),
			APIKey:      getEnv("OPNSENSE_API_KEY", ""),
			APISecret:   getEnv("OPNSENSE_API_SECRET", ""),
			SSHHost:     getEnv("OPNSENSE_SSH_HOST", "10.10.10.60:22"),
			SSHUser:     getEnv("OPNSENSE_SSH_USER", "root"),
			SSHPassword: getEnv("OPNSENSE_SSH_PASSWORD", ""),
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
