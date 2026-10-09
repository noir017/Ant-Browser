package mcpserver

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Host 头白名单。
//
// SDK 自带 DNS rebinding 防护：连接落在回环地址、而 Host 不是回环时直接 403。
// 本机直连没问题，但应用常被放在转发之后——容器里的 socat relay、ssh -R 之类——
// 这时所有请求都从 127.0.0.1 进来，Host 却是对外地址（如 10.192.168.31:19877），
// 于是一律被拒，MCP 只能在本机用。
//
// 这里不整个关掉防护，而是允许显式列出的主机名：rebinding 攻击的 Host 是攻击者
// 自己的域名，不会落在白名单里，防护依旧有效。白名单为空时完全沿用 SDK 的行为。

// normalizeAllowedHosts 把配置里的主机名统一成小写、去掉端口和 IPv6 方括号，丢弃空项。
// 端口不参与比对：relay 的对外端口与应用实际监听端口本来就不同。
func normalizeAllowedHosts(hosts []string) map[string]struct{} {
	allowed := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		if name := hostnameOf(host); name != "" {
			allowed[name] = struct{}{}
		}
	}
	return allowed
}

// hostnameOf 从 "host"、"host:port"、"[v6]"、"[v6]:port" 里取出小写主机名。
func hostnameOf(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	return strings.ToLower(strings.Trim(value, "[]"))
}

// isLoopbackHost 与 SDK 的判定保持一致：localhost 或回环 IP。
func isLoopbackHost(value string) bool {
	host := hostnameOf(value)
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}

// withAllowedHosts 用白名单版的 rebinding 防护包住 next。
// 调用方需要同时关掉 SDK 自带的检查（DisableLocalhostProtection），否则白名单不起作用。
func withAllowedHosts(next http.Handler, allowed map[string]struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		localAddr, ok := req.Context().Value(http.LocalAddrContextKey).(net.Addr)
		if ok && localAddr != nil && isLoopbackHost(localAddr.String()) && !isLoopbackHost(req.Host) {
			if _, permitted := allowed[hostnameOf(req.Host)]; !permitted {
				http.Error(w, fmt.Sprintf("Forbidden: invalid Host header %q", req.Host), http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, req)
	})
}
