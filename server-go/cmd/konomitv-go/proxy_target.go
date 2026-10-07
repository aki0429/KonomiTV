package main

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// validateProxyTarget は明らかに HTTP サーバー自身を指す転送先を起動前に拒否する。
// DNS 別名や外部リバースプロキシ経由の循環までは判定しない。
// 移行完了検証では -no-proxy を使い、fallback 自体を無効化する。
func validateProxyTarget(listenAddress, backendURL string) error {
	return validateProxyTargetWithInterfaces(listenAddress, backendURL, localProxyInterfaces)
}

// NIC の所属情報を保持し、判定テストでは OS の実インターフェースを使わない。
type proxyInterface struct {
	name      string
	index     int
	addresses []netip.Addr
}

func validateProxyTargetWithInterfaces(listenAddress, backendURL string, discover func() ([]proxyInterface, error)) error {
	if backendURL == "" {
		return nil
	}
	// net/http.Server.ListenAndServe と同じく、空 Addr は :http とする。
	if listenAddress == "" {
		listenAddress = ":http"
	}
	listenHost, listenPort, err := net.SplitHostPort(listenAddress)
	if err != nil {
		return fmt.Errorf("invalid listen address for proxy target validation")
	}
	// net/http と同じ TCP ポート解釈で、サービス名と動的ポートも受理する。
	listenNumber, err := net.LookupPort("tcp", listenPort)
	if err != nil {
		return fmt.Errorf("invalid listen address for proxy target validation: %w", err)
	}
	backend, err := url.Parse(backendURL)
	if err != nil || backend.Hostname() == "" || (backend.Scheme != "http" && backend.Scheme != "https") {
		// URL は認証情報を含む可能性があるためエラーに出力しない。
		return fmt.Errorf("python backend must be an absolute HTTP or HTTPS URL")
	}
	backendPort := backend.Port()
	if backendPort == "" {
		backendPort = "80"
		if backend.Scheme == "https" {
			backendPort = "443"
		}
	}
	if !validProxyPort(backendPort) {
		return fmt.Errorf("invalid python backend port")
	}
	backendNumber, _ := strconv.Atoi(backendPort)
	// 0 と空ポートは OS が選ぶため、固定 backend ポートとの一致は判定できない。
	if listenNumber == 0 || listenNumber != backendNumber {
		return nil
	}
	// IPv6 の zone は NIC 名であり大文字小文字を区別するため、ここでは変更しない。
	backendHost := backend.Hostname()
	listenIP, listenIsIP := proxyHostIP(listenHost)
	backendIP, backendIsIP := proxyHostIP(backendHost)
	wildcard := listenHost == "" || (listenIsIP && listenIP.IsUnspecified())
	// 異なる zone 表記は数値風でも実在する NIC 名かもしれないため、
	// NIC snapshot を取得する前に数値 fallback で自己判定を確定しない。
	scopedPair := listenIsIP && backendIsIP && listenIP != backendIP && listenIP.Zone() != "" && backendIP.Zone() != "" && listenIP.WithZone("") == backendIP.WithZone("")
	self := false
	if !scopedPair {
		self = proxyTargetIsSelf(listenHost, backendHost, nil)
	}
	if !self && backendIsIP && (wildcard || scopedPair) {
		interfaces, err := discover()
		if err != nil {
			return fmt.Errorf("cannot validate local proxy target addresses: %w", err)
		}
		// 列挙成功なら nil の戻り値も既知の空集合として扱う。
		if interfaces == nil {
			interfaces = []proxyInterface{}
		}
		self = proxyTargetIsSelf(listenHost, backendHost, interfaces)
	}
	if self {
		return fmt.Errorf("python backend points to this Go listener; use -no-proxy or a distinct Python backend port")
	}
	return nil
}

// OS の NIC 列挙だけを分離し、インターフェース名・index と各数値 IP を保持する。
func localProxyInterfaces() ([]proxyInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	local := make([]proxyInterface, 0, len(interfaces))
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			return nil, err
		}
		entry := proxyInterface{name: iface.Name, index: iface.Index}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err == nil {
				entry.addresses = append(entry.addresses, prefix.Addr().Unmap())
			}
		}
		local = append(local, entry)
	}
	return local, nil
}

// proxyTargetIsSelf は与えられた NIC 情報のみで判定し、OS や DNS を参照しない。
func proxyTargetIsSelf(listenHost, backendHost string, interfaces []proxyInterface) bool {
	listenIP, listenIsIP := proxyHostIP(listenHost)
	backendIP, backendIsIP := proxyHostIP(backendHost)
	listenName := strings.ToLower(strings.TrimSuffix(listenHost, "."))
	backendName := strings.ToLower(strings.TrimSuffix(backendHost, "."))
	if (!listenIsIP && !backendIsIP && listenName == backendName) || (listenIsIP && backendIsIP && listenIP == backendIP) {
		return true
	}
	if listenIsIP && backendIsIP && listenIP.Zone() != "" && backendIP.Zone() != "" && listenIP.WithZone("") == backendIP.WithZone("") {
		listenScope := proxyZoneIndex(listenIP.Zone(), interfaces)
		if listenScope > 0 && listenScope == proxyZoneIndex(backendIP.Zone(), interfaces) {
			return true
		}
	}
	wildcard := listenHost == "" || (listenIsIP && listenIP.IsUnspecified())
	if !wildcard {
		return false
	}
	if backendName == "localhost" || (backendIsIP && (backendIP.IsLoopback() || backendIP.IsUnspecified())) {
		return true
	}
	if !backendIsIP {
		return false
	}
	zone := backendIP.Zone()
	scope := proxyZoneIndex(zone, interfaces)
	for _, iface := range interfaces {
		// zone 付きの宛先は、その NIC に同じ IP が所属している場合だけ自己とする。
		if zone != "" && (scope <= 0 || scope != iface.index) {
			continue
		}
		for _, address := range iface.addresses {
			if address.WithZone("").Unmap() == backendIP.WithZone("").Unmap() {
				return true
			}
		}
	}
	return false
}

// scope 名を優先して index に解決し、大小文字と数値風の NIC 名を保持する。
// nil は NIC 情報が未知なので fallback せず、空の既知集合とは区別する。
func proxyZoneIndex(zone string, interfaces []proxyInterface) int {
	if zone == "" || interfaces == nil {
		return 0
	}
	for _, iface := range interfaces {
		if zone == iface.name {
			return iface.index
		}
	}
	// net.ipv6ZoneCache.index は dtoi の数値のみを使うため、十進 prefix と
	// 0xFFFFFF での飽和を再現する (消費文字数と成功フラグは判定しない)。
	const maxIndex = 0xFFFFFF
	index := 0
	for i := 0; i < len(zone) && '0' <= zone[i] && zone[i] <= '9'; i++ {
		index = index*10 + int(zone[i]-'0')
		if index >= maxIndex {
			return maxIndex
		}
	}
	return index
}

func validProxyPort(value string) bool {
	port, err := strconv.Atoi(value)
	return err == nil && port > 0 && port <= 65535
}

func proxyHostIP(host string) (netip.Addr, bool) {
	address, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return address.Unmap(), true
}
