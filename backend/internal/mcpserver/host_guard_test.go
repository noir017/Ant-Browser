package mcpserver

import (
	"net/http"
	"strings"
	"testing"
)

// Host 白名单的测试。
//
// httptest 服务监听 127.0.0.1，正好复现 relay 场景：本地地址是回环，
// 而客户端带来的 Host 是对外地址。

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"host-test","version":"0"}}}`

func postInitializeWithHost(t *testing.T, endpoint string, host string) int {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, endpoint, strings.NewReader(initializeBody))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Host = host
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post initialize: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestDefaultRejectsForwardedHost(t *testing.T) {
	endpoint := newHTTPTestServer(t, &fakeProvider{}, Options{})

	if status := postInitializeWithHost(t, endpoint, "10.192.168.31:19877"); status != http.StatusForbidden {
		t.Fatalf("without allowed hosts a non-loopback Host must be rejected, got %d", status)
	}
	if status := postInitializeWithHost(t, endpoint, "localhost:19876"); status != http.StatusOK {
		t.Fatalf("loopback Host must still work, got %d", status)
	}
}

func TestAllowedHostsPermitsListedHostOnly(t *testing.T) {
	endpoint := newHTTPTestServer(t, &fakeProvider{}, Options{AllowedHosts: []string{"10.192.168.31"}})

	cases := []struct {
		host string
		want int
	}{
		{"10.192.168.31:19877", http.StatusOK},
		{"10.192.168.31", http.StatusOK},
		{"127.0.0.1:19876", http.StatusOK},
		{"localhost", http.StatusOK},
		// DNS rebinding：攻击者域名解析到同一地址，但 Host 是它自己的名字。
		{"rebind.attacker.example:19877", http.StatusForbidden},
		{"10.192.168.32:19877", http.StatusForbidden},
		{"10.192.168.31.attacker.example", http.StatusForbidden},
	}
	for _, tc := range cases {
		if status := postInitializeWithHost(t, endpoint, tc.host); status != tc.want {
			t.Errorf("Host %q: got %d, want %d", tc.host, status, tc.want)
		}
	}
}

func TestAllowedHostsIgnoresPortCaseAndBrackets(t *testing.T) {
	endpoint := newHTTPTestServer(t, &fakeProvider{}, Options{
		AllowedHosts: []string{" Browser.Mesh.Example:19877 ", "[fd00::31]", ""},
	})

	for _, host := range []string{"browser.mesh.example:8080", "BROWSER.MESH.EXAMPLE", "[fd00::31]:19877"} {
		if status := postInitializeWithHost(t, endpoint, host); status != http.StatusOK {
			t.Errorf("Host %q should match the allow list, got %d", host, status)
		}
	}
}

func TestAllowedHostsSessionWorksEndToEnd(t *testing.T) {
	endpoint := newHTTPTestServer(t, &fakeProvider{}, Options{AllowedHosts: []string{"127.0.0.1"}})

	session := connectOverHTTP(t, endpoint)
	if _, err := session.ListTools(t.Context(), nil); err != nil {
		t.Fatalf("list tools through the allow-listed handler: %v", err)
	}
}
