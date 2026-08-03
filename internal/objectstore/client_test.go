package objectstore

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		Endpoint:  "s3.lab.jmal.io",
		AccessKey: "AKIDEXAMPLE",
		SecretKey: "supersecretkey",
		Bucket:    "crucible-images",
		Prefix:    "crucible",
		UseSSL:    true,
	}
}

func TestNew_ValidatesConfig(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantErr bool
	}{
		{"valid", func(c *Config) {}, false},
		{"empty endpoint", func(c *Config) { c.Endpoint = "" }, true},
		{"whitespace endpoint", func(c *Config) { c.Endpoint = "   " }, true},
		{"empty bucket", func(c *Config) { c.Bucket = "" }, true},
		{"empty access key", func(c *Config) { c.AccessKey = "" }, true},
		{"empty secret key", func(c *Config) { c.SecretKey = "" }, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			tt.mutate(&cfg)

			client, err := New(cfg)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("New(%+v) = nil error, want error", cfg)
				}
				if client != nil {
					t.Fatalf("New returned non-nil client alongside error: %v", client)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%+v) unexpected error: %v", cfg, err)
			}
			if client == nil {
				t.Fatal("New returned nil client without error")
			}
		})
	}
}

func TestNew_DerivesSSLFromEndpointScheme(t *testing.T) {
	tests := []struct {
		name         string
		endpoint     string
		cfgUseSSL    bool
		wantEndpoint string
		wantUseSSL   bool
	}{
		{"https scheme forces ssl", "https://host", false, "host", true},
		{"https scheme with port", "https://host:9000", false, "host:9000", true},
		{"http scheme forces plaintext", "http://host:9000", true, "host:9000", false},
		{"bare host honours explicit ssl true", "host:9000", true, "host:9000", true},
		{"bare host honours explicit ssl false", "host:9000", false, "host:9000", false},
		{"https scheme trailing slash trimmed", "https://host/", false, "host", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Endpoint = tt.endpoint
			cfg.UseSSL = tt.cfgUseSSL

			client, err := New(cfg)
			if err != nil {
				t.Fatalf("New(endpoint=%q) unexpected error: %v", tt.endpoint, err)
			}
			if client.endpoint != tt.wantEndpoint {
				t.Errorf("endpoint = %q, want %q", client.endpoint, tt.wantEndpoint)
			}
			if client.useSSL != tt.wantUseSSL {
				t.Errorf("useSSL = %v, want %v", client.useSSL, tt.wantUseSSL)
			}
		})
	}
}

func TestKey_NeverEscapesPrefix(t *testing.T) {
	const prefix = "crucible"
	c := &Client{prefix: prefix}
	want := prefix + "/"

	nasty := []string{
		"../../etc/passwd",
		"..\\..\\windows",
		"/abs",
		"a/../../b",
		"",
		strings.Repeat("../", 50) + "x",
		"....//....//x",
		"foo/../../../bar",
		"\\\\server\\share",
	}

	for _, in := range nasty {
		got := c.Key(in)
		if !strings.HasPrefix(got, want) {
			t.Errorf("Key(%q) = %q, does not start with %q", in, got, want)
		}
		if strings.Contains(got, "..") {
			t.Errorf("Key(%q) = %q, contains %q", in, got, "..")
		}
		if strings.Contains(got, "\\") {
			t.Errorf("Key(%q) = %q, contains a backslash", in, got)
		}
	}
}

func TestKey_JoinsAndCleans(t *testing.T) {
	c := &Client{prefix: "crucible"}

	tests := []struct {
		name  string
		parts []string
		want  string
	}{
		{"single part", []string{"images"}, "crucible/images"},
		{"multiple parts", []string{"images", "ubuntu.iso"}, "crucible/images/ubuntu.iso"},
		{"nested parts", []string{"a", "b", "c"}, "crucible/a/b/c"},
		{"strips surrounding slashes", []string{"a/", "/b/"}, "crucible/a/b"},
		{"collapses doubled slashes", []string{"a//b"}, "crucible/a/b"},
		{"drops dot segments", []string{"a", ".", "b"}, "crucible/a/b"},
		{"normalises backslashes", []string{"a\\b"}, "crucible/a/b"},
		{"empty produces prefix root", []string{""}, "crucible/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.Key(tt.parts...)
			if got != tt.want {
				t.Errorf("Key(%q) = %q, want %q", tt.parts, got, tt.want)
			}
			if strings.Contains(got, "//") {
				t.Errorf("Key(%q) = %q, contains a doubled slash", tt.parts, got)
			}
		})
	}
}

func TestKey_EmptyPrefix(t *testing.T) {
	c := &Client{prefix: ""}
	if got := c.Key("a", "b"); got != "a/b" {
		t.Errorf("Key with empty prefix = %q, want %q", got, "a/b")
	}
}

func TestClampPresignTTL(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want time.Duration
	}{
		{"over max clamps to 15m", 24 * time.Hour, MaxPresignTTL},
		{"exactly max unchanged", MaxPresignTTL, MaxPresignTTL},
		{"just over max clamps", MaxPresignTTL + time.Second, MaxPresignTTL},
		{"under max passes through", 5 * time.Minute, 5 * time.Minute},
		{"one second passes through", time.Second, time.Second},
		{"zero gets default", 0, defaultPresignTTL},
		{"negative gets default", -3 * time.Hour, defaultPresignTTL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := clampPresignTTL(tt.in)
			if got != tt.want {
				t.Errorf("clampPresignTTL(%v) = %v, want %v", tt.in, got, tt.want)
			}
			if got <= 0 {
				t.Errorf("clampPresignTTL(%v) = %v, must be positive", tt.in, got)
			}
			if got > MaxPresignTTL {
				t.Errorf("clampPresignTTL(%v) = %v, exceeds MaxPresignTTL %v", tt.in, got, MaxPresignTTL)
			}
		})
	}
}
