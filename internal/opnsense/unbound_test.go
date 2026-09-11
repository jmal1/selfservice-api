package opnsense

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGetDNSBLPolicy_ParsesOPNsense26ModelShape(t *testing.T) {
	const body = `{
	  "blocklist": {
	    "enabled": "1",
	    "type": {
	      "oisd2": {"value":"NSFW Blocklist","selected":1},
	      "hgz014": {"value":"DoH/VPN/TOR/Proxy Bypass","selected":"1"},
	      "hgz021": {"value":"Gambling - Mini","selected":1}
	    },
	    "lists": "https://filter.internal.example/ut1.txt",
	    "allowlists": "classroom.example",
	    "blocklists": "",
	    "wildcards": "",
	    "source_nets": {
	      "10.100.0.0/16": {"value":"10.100.0.0/16","selected":1},
	      "10.200.0.0/16": {"value":"10.200.0.0/16","selected":"1"},
	      "10.250.0.0/16": {"value":"10.250.0.0/16","selected":0}
	    },
	    "address": "",
	    "nxdomain": "1",
	    "cache_ttl": "3600",
	    "description": "crucible:content-filter:v1"
	  }
	}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/unbound/settings/getDnsbl/policy-uuid" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())
	got, err := c.GetDNSBLPolicy(context.Background(), "policy-uuid")
	if err != nil {
		t.Fatalf("GetDNSBLPolicy: %v", err)
	}
	if got.Types != "hgz014,hgz021,oisd2" {
		t.Fatalf("Types = %q", got.Types)
	}
	if got.SourceNets != "10.100.0.0/16,10.200.0.0/16" || got.Lists != "https://filter.internal.example/ut1.txt" {
		t.Fatalf("policy scope/feed not parsed: %+v", got)
	}
}

func TestSetUnboundSafeSearch_UsesGeneralModelAndValidatesResponse(t *testing.T) {
	var payload map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/unbound/settings/set" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		_, _ = io.WriteString(w, `{"result":"saved"}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())
	if err := c.SetUnboundSafeSearch(context.Background(), true); err != nil {
		t.Fatalf("SetUnboundSafeSearch: %v", err)
	}
	unbound := payload["unbound"].(map[string]any)
	general := unbound["general"].(map[string]any)
	if general["safesearch"] != "1" {
		t.Fatalf("safesearch payload = %#v", general["safesearch"])
	}
}

func TestListDNSBLPolicies_RequestsAndRequiresCompleteInventory(t *testing.T) {
	var payload struct {
		Current  int `json:"current"`
		RowCount int `json:"rowCount"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/unbound/settings/searchDnsbl" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		_, _ = io.WriteString(w, `{"total":2,"rows":[{"uuid":"one","enabled":"1","description":"manual"}]}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())
	if _, err := c.ListDNSBLPolicies(context.Background()); err == nil {
		t.Fatal("truncated DNSBL search inventory must fail closed")
	}
	if payload.Current != 1 || payload.RowCount != 1000 {
		t.Fatalf("pagination payload = %+v", payload)
	}
}

func TestCreateDNSBLPolicy_RejectsValidationFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"validations":{"blocklist.source_nets":"invalid"}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())
	if _, err := c.CreateDNSBLPolicy(context.Background(), DNSBLPolicy{
		Enabled: "1", SourceNets: "bad", Description: "crucible:content-filter:v1",
	}); err == nil {
		t.Fatal("CreateDNSBLPolicy succeeded despite OPNsense validation error")
	}
}

func TestDeleteDNSBLPolicy_AcceptsDeletedAndRefreshRejectsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/unbound/settings/delDnsbl/policy":
			_, _ = io.WriteString(w, `{"result":"deleted"}`)
		case "/unbound/service/dnsbl":
			_, _ = io.WriteString(w, `{"status":"failed"}`)
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())
	if err := c.DeleteDNSBLPolicy(context.Background(), "policy"); err != nil {
		t.Fatalf("DeleteDNSBLPolicy: %v", err)
	}
	if err := c.RefreshUnboundDNSBL(context.Background()); err == nil {
		t.Fatal("HTTP 200 backend failure must not count as DNSBL refresh")
	}
}

func TestGetUnboundSafeSearch_RejectsUnknownValue(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"unbound":{"general":{"safesearch":"maybe"}}}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL}, discardLogger())
	if _, err := c.GetUnboundSafeSearch(context.Background()); err == nil {
		t.Fatal("unknown safesearch value must fail closed")
	}
}
