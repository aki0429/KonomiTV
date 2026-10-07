package main

import (
	"context"
	"errors"
	"flag"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 自己プロキシは未実装 API から同じサーバーへ再帰し続けるため、起動前に拒否する。
func TestValidateProxyTarget(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr bool
	}{
		{"proxy disabled", "127.0.0.77:7010", "", false},
		{"parallel ports", "0.0.0.0:7002", "http://127.0.0.77:7010/", false},
		{"exact self", "127.0.0.77:7010", "http://127.0.0.77:7010/", true},
		{"wildcard loopback self", "0.0.0.0:7010", "http://127.0.0.77:7010/", true},
		{"empty wildcard localhost self", ":7010", "http://localhost:7010/", true},
		{"same host name", "localhost:7010", "http://localhost:7010/", true},
		{"IPv6 self", "[::1]:7010", "http://[::1]:7010/", true},
		{"IPv4 mapped self", "127.0.0.1:7010", "http://[::ffff:127.0.0.1]:7010/", true},
		{"other loopback address", "127.0.0.77:7010", "http://127.0.0.1:7010/", false},
		{"remote host same port", "0.0.0.0:7010", "http://192.0.2.1:7010/", false},
		{"default HTTP port self", "127.0.0.1:80", "http://127.0.0.1/", true},
		{"default HTTPS port self", "127.0.0.1:443", "https://127.0.0.1/", true},
		{"relative URL", "127.0.0.1:7010", "/api/", true},
		{"unsupported scheme", "127.0.0.1:7010", "file:///etc/passwd", true},
		{"invalid backend port", "127.0.0.1:7010", "http://127.0.0.1:invalid/", true},
		{"invalid listener", "not-an-address", "http://127.0.0.1:7010/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProxyTarget(tc.listen, tc.backend)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateProxyTarget error=%v, want error=%v", err, tc.wantErr)
			}
		})
	}
}

// listener のポート解釈は net.Listen と揃え、動的ポートを固定転送先と誤認しない。
func TestValidateProxyTargetListenerPorts(t *testing.T) {
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr string
	}{
		{"dynamic wildcard", ":0", "http://127.0.0.1:7010/", ""},
		{"dynamic exact host", "127.0.0.1:0", "http://127.0.0.1:7010/", ""},
		{"empty dynamic port", "127.0.0.1:", "http://127.0.0.1:7010/", ""},
		{"empty wildcard port", ":", "http://127.0.0.1:7010/", ""},
		{"HTTP service distinct port", "127.0.0.1:http", "http://127.0.0.1:7010/", ""},
		{"HTTPS service distinct port", "127.0.0.1:https", "https://127.0.0.1:7010/", ""},
		{"HTTP service self", "127.0.0.1:http", "http://127.0.0.1/", "points to this Go listener"},
		{"HTTPS service self", "127.0.0.1:https", "https://127.0.0.1/", "points to this Go listener"},
		{"wildcard HTTP service self", ":http", "http://localhost/", "points to this Go listener"},
		{"negative port", "127.0.0.1:-1", "http://127.0.0.1:7010/", "invalid listen address"},
		{"out of range port", "127.0.0.1:65536", "http://127.0.0.1:7010/", "invalid listen address"},
		{"unknown service", "127.0.0.1:konomitv-invalid-test-service", "http://127.0.0.1:7010/", "invalid listen address"},
		{"zero backend port", ":0", "http://127.0.0.1:0/", "invalid python backend port"},
		{"out of range backend port", "127.0.0.1:", "http://127.0.0.1:65536/", "invalid python backend port"},
		{"service backend port", "127.0.0.1:0", "http://127.0.0.1:http/", "absolute HTTP or HTTPS URL"},
		{"dynamic listener relative backend", ":0", "/api/", "absolute HTTP or HTTPS URL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProxyTarget(tc.listen, tc.backend)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("listener accepted by net.Listen must remain allowed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validateProxyTarget error=%v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// net/http.Server.ListenAndServe は空の Addr を :http として扱う。
func TestValidateProxyTargetEmptyHTTPServerAddress(t *testing.T) {
	cases := []struct {
		name    string
		backend string
		wantErr string
	}{
		{"distinct backend port", "http://127.0.0.1:7010/", ""},
		{"default HTTP self", "http://127.0.0.1/", "points to this Go listener"},
		{"proxy disabled", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			discover := func() ([]proxyInterface, error) {
				t.Fatal("distinct ports, obvious loopback self, and disabled proxy must not enumerate NICs")
				return nil, nil
			}
			err := validateProxyTargetWithInterfaces("", tc.backend, discover)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("empty HTTP Server.Addr must remain allowed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

// IPNet は zone を保持しないので、所属する NIC の名前と index を合成する。
func syntheticProxyInterfaces(t *testing.T) []proxyInterface {
	t.Helper()
	localAddress, linkLocal, err := net.ParseCIDR("fe80::1234/64")
	if err != nil {
		t.Fatal(err)
	}
	otherAddress, otherLink, err := net.ParseCIDR("fe80::5678/64")
	if err != nil {
		t.Fatal(err)
	}
	// Interface.Addrs の IPNet と同様に、ネットワークではなくホスト IP を持たせる。
	linkLocal.IP = localAddress
	otherLink.IP = otherAddress
	localPrefix, err := netip.ParsePrefix(linkLocal.String())
	if err != nil {
		t.Fatal(err)
	}
	otherPrefix, err := netip.ParsePrefix(otherLink.String())
	if err != nil {
		t.Fatal(err)
	}
	localIP := localPrefix.Addr()
	otherIP := otherPrefix.Addr()
	return []proxyInterface{
		{name: "EtherTest", index: 7, addresses: []netip.Addr{localIP, netip.MustParseAddr("2001:db8::1234"), netip.MustParseAddr("192.0.2.10")}},
		{name: "OtherTest", index: 8, addresses: []netip.Addr{otherIP}},
	}
}

// ワイルドカード待受は、対象 IP が実際に所属する scope の場合だけ自己転送になる。
func TestValidateProxyTargetScopedWildcard(t *testing.T) {
	interfaces := syntheticProxyInterfaces(t)
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr bool
	}{
		{"interface name self", "[::]:7010", "http://[fe80::1234%25EtherTest]:7010/", true},
		{"interface index self", "[::]:7010", "http://[fe80::1234%257]:7010/", true},
		{"zero padded interface index self", "[::]:7010", "http://[fe80::1234%25007]:7010/", true},
		{"empty wildcard scoped self", ":7010", "http://[fe80::1234%25EtherTest]:7010/", true},
		{"different interface name", "[::]:7010", "http://[fe80::1234%25OtherTest]:7010/", false},
		{"different interface index", "[::]:7010", "http://[fe80::1234%258]:7010/", false},
		{"unknown interface name", "[::]:7010", "http://[fe80::1234%25MissingTest]:7010/", false},
		{"unknown interface index", "[::]:7010", "http://[fe80::1234%2599]:7010/", false},
		{"case sensitive interface name", "[::]:7010", "http://[fe80::1234%25ethertest]:7010/", false},
		{"different link local IP", "[::]:7010", "http://[fe80::9999%25EtherTest]:7010/", false},
		{"distinct port", "[::]:7010", "http://[fe80::1234%25EtherTest]:7011/", false},
		{"unscoped global IPv6 self", "[::]:7010", "http://[2001:db8::1234]:7010/", true},
		{"unscoped IPv4 self", "0.0.0.0:7010", "http://192.0.2.10:7010/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			discover := func() ([]proxyInterface, error) { return interfaces, nil }
			err := validateProxyTargetWithInterfaces(tc.listen, tc.backend, discover)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "points to this Go listener") {
					t.Fatalf("scoped self-proxy must be rejected, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("different endpoint must remain allowed: %v", err)
			}
			listenHost, _, err := net.SplitHostPort(tc.listen)
			if err != nil {
				t.Fatal(err)
			}
			backend, err := url.Parse(tc.backend)
			if err != nil {
				t.Fatal(err)
			}
			// ポート差を除き、純粋 helper にも同じ scope 判定を要求する。
			if tc.name != "distinct port" {
				if got := proxyTargetIsSelf(listenHost, backend.Hostname(), interfaces); got != tc.wantErr {
					t.Fatalf("pure scope comparison=%v, want %v", got, tc.wantErr)
				}
			}
		})
	}
}

// 同じ NIC の zone 名と数値 index は、指定 IP 待受でも同じ scope を表す。
func TestValidateProxyTargetEquivalentScopes(t *testing.T) {
	interfaces := syntheticProxyInterfaces(t)
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr bool
	}{
		{"name listener index backend", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%257]:7010/", true},
		{"index listener name backend", "[fe80::1234%7]:7010", "http://[fe80::1234%25EtherTest]:7010/", true},
		{"same scope literal", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%25EtherTest]:7010/", true},
		{"different named scope", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%25OtherTest]:7010/", false},
		{"different numeric scope", "[fe80::1234%7]:7010", "http://[fe80::1234%258]:7010/", false},
		{"case sensitive bound scope", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%25ethertest]:7010/", false},
		{"different IP same scope", "[fe80::1234%EtherTest]:7010", "http://[fe80::5678%257]:7010/", false},
		{"same scope different port", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%257]:7011/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			discover := func() ([]proxyInterface, error) { return interfaces, nil }
			err := validateProxyTargetWithInterfaces(tc.listen, tc.backend, discover)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "points to this Go listener") {
					t.Fatalf("same scope must reject self-proxy, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("different endpoint must remain allowed: %v", err)
			}
		})
	}
}

// net の zone fallback は十進 prefix のみを読み、0xFFFFFF で飽和する。
func TestValidateProxyTargetNumericPrefixScopes(t *testing.T) {
	interfaces := syntheticProxyInterfaces(t)
	interfaces = append(interfaces,
		proxyInterface{name: "NearMaxTest", index: 0xFFFFFE, addresses: []netip.Addr{netip.MustParseAddr("fe80::1234")}},
		proxyInterface{name: "SaturatedTest", index: 0xFFFFFF, addresses: []netip.Addr{netip.MustParseAddr("fe80::1234")}},
	)
	cases := []struct {
		name    string
		listen  string
		zone    string
		wantErr bool
	}{
		{"wildcard prefix self", "[::]:7010", "7suffix", true},
		{"bound name prefix self", "[fe80::1234%EtherTest]:7010", "7suffix", true},
		{"wildcard leading zero prefix self", "[::]:7010", "007suffix", true},
		{"bound leading zero prefix self", "[fe80::1234%EtherTest]:7010", "007suffix", true},
		{"wildcard different prefix", "[::]:7010", "8suffix", false},
		{"bound different prefix", "[fe80::1234%EtherTest]:7010", "8suffix", false},
		{"negative prefix", "[::]:7010", "-7suffix", false},
		{"positive sign prefix", "[::]:7010", "+7suffix", false},
		{"nondigit prefix", "[::]:7010", "suffix7", false},
		{"zero prefix", "[::]:7010", "0suffix", false},
		{"below saturation self", "[fe80::1234%NearMaxTest]:7010", "16777214suffix", true},
		{"at saturation self", "[::]:7010", "16777215suffix", true},
		{"above saturation self", "[::]:7010", "16777216suffix", true},
		{"bound saturated prefix self", "[fe80::1234%SaturatedTest]:7010", "16777216suffix", true},
		{"large decimal self", "[::]:7010", "999999999999999999999999suffix", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			discover := func() ([]proxyInterface, error) { return interfaces, nil }
			backend := "http://[fe80::1234%25" + tc.zone + "]:7010/"
			err := validateProxyTargetWithInterfaces(tc.listen, backend, discover)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "points to this Go listener") {
					t.Fatalf("Go numeric-prefix scope must reject self-proxy, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("different or unresolved scope must remain allowed: %v", err)
			}
		})
	}
}

// 実在する prefix 風 NIC 名は、数値 fallback より優先される。
func TestValidateProxyTargetNumericPrefixInterfaceName(t *testing.T) {
	interfaces := syntheticProxyInterfaces(t)
	interfaces[1].name = "7suffix"
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr bool
	}{
		{"named prefix different IP owner", "[::]:7010", "http://[fe80::1234%257suffix]:7010/", false},
		{"named prefix actual owner", "[::]:7010", "http://[fe80::5678%257suffix]:7010/", true},
		{"name priority over prefix", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%257suffix]:7010/", false},
		{"bound numeric index versus prefix name", "[fe80::1234%7]:7010", "http://[fe80::1234%257suffix]:7010/", false},
		{"bound prefix name versus numeric index", "[fe80::1234%7suffix]:7010", "http://[fe80::1234%257]:7010/", false},
		{"named prefix equivalent actual index", "[fe80::5678%7suffix]:7010", "http://[fe80::5678%258suffix]:7010/", true},
		{"prefix name case sensitivity", "[::]:7010", "http://[fe80::1234%257SUFFIX]:7010/", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			discover := func() ([]proxyInterface, error) {
				calls++
				return interfaces, nil
			}
			err := validateProxyTargetWithInterfaces(tc.listen, tc.backend, discover)
			if calls != 1 {
				t.Fatalf("NIC snapshot must be resolved exactly once before the verdict, calls=%d", calls)
			}
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "points to this Go listener") {
					t.Fatalf("same scope must reject self-proxy, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("different interface scope must remain allowed: %v", err)
			}
		})
	}
}

// net の zone 解決は NIC 名を優先するので、数値に見える別 NIC 名を index と誤認しない。
func TestValidateProxyTargetNumericInterfaceName(t *testing.T) {
	interfaces := syntheticProxyInterfaces(t)
	interfaces[1].name = "7"
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr bool
	}{
		{"numeric name different IP owner", "[::]:7010", "http://[fe80::1234%257]:7010/", false},
		{"numeric name actual owner", "[::]:7010", "http://[fe80::5678%257]:7010/", true},
		{"name takes priority over index", "[fe80::1234%EtherTest]:7010", "http://[fe80::1234%257]:7010/", false},
		{"numeric name equivalent actual index", "[fe80::5678%7]:7010", "http://[fe80::5678%258]:7010/", true},
		{"numeric name versus padded numeric index", "[fe80::1234%7]:7010", "http://[fe80::1234%25007]:7010/", false},
		{"padded numeric index versus numeric name", "[fe80::1234%007]:7010", "http://[fe80::1234%257]:7010/", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			discover := func() ([]proxyInterface, error) { return interfaces, nil }
			err := validateProxyTargetWithInterfaces(tc.listen, tc.backend, discover)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "points to this Go listener") {
					t.Fatalf("same scope must reject self-proxy, got %v", err)
				}
			} else if err != nil {
				t.Fatalf("different interface scope must remain allowed: %v", err)
			}
		})
	}
}

// nil は未知の NIC 情報、非 nil の空 slice は列挙済みで NIC がない状態を表す。
func TestProxyTargetIsSelfRequiresKnownScopeSnapshot(t *testing.T) {
	const listen = "fe80::1234%7"
	const backend = "fe80::1234%7suffix"
	if proxyTargetIsSelf(listen, backend, nil) {
		t.Fatal("unknown NIC names must not confirm a tentative numeric-prefix match")
	}
	if !proxyTargetIsSelf(listen, backend, []proxyInterface{}) {
		t.Fatal("a known empty NIC snapshot must permit Go's numeric-prefix fallback")
	}
}

// 列挙成功の nil slice も既知の空集合であり、未知の snapshot と混同しない。
func TestValidateProxyTargetEmptyInterfaceSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name       string
		interfaces []proxyInterface
	}{
		{"empty slice", []proxyInterface{}},
		{"nil result", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			discover := func() ([]proxyInterface, error) {
				calls++
				return tc.interfaces, nil
			}
			err := validateProxyTargetWithInterfaces("[fe80::1234%7]:7010", "http://[fe80::1234%257suffix]:7010/", discover)
			if calls != 1 {
				t.Fatalf("NIC snapshot must precede numeric fallback exactly once, calls=%d", calls)
			}
			if err == nil || !strings.Contains(err.Error(), "points to this Go listener") {
				t.Fatalf("known empty snapshot must use Go's numeric-prefix fallback, got %v", err)
			}
		})
	}
}

// 判定に必要な NIC 列挙の失敗は拒否し、別ポートと無効 proxy は列挙しない。
func TestValidateProxyTargetInterfaceDiscovery(t *testing.T) {
	cases := []struct {
		name      string
		listen    string
		backend   string
		wantErr   string
		wantCalls int
	}{
		{"enumeration failure", "[::]:7010", "http://[fe80::1234%257suffix]:7010/", "cannot validate local proxy target addresses", 1},
		{"bound numeric prefix needs snapshot first", "[fe80::1234%7]:7010", "http://[fe80::1234%257suffix]:7010/", "cannot validate local proxy target addresses", 1},
		{"bound prefix numeric needs snapshot first", "[fe80::1234%7suffix]:7010", "http://[fe80::1234%257]:7010/", "cannot validate local proxy target addresses", 1},
		{"numeric literals need snapshot first", "[fe80::1234%7]:7010", "http://[fe80::1234%25007]:7010/", "cannot validate local proxy target addresses", 1},
		{"distinct ports skip enumeration", "[::]:7010", "http://[fe80::1234%257suffix]:7011/", "", 0},
		{"disabled proxy skip enumeration", "not-an-address", "", "", 0},
		{"dynamic port skips enumeration", "[fe80::1234%7]:0", "http://[fe80::1234%257suffix]:7010/", "", 0},
		{"empty port skips enumeration", "[fe80::1234%7]:", "http://[fe80::1234%257suffix]:7010/", "", 0},
		{"exact hostname skips enumeration", "LOCALHOST.:7010", "http://localhost:7010/", "points to this Go listener", 0},
		{"exact IP and zone skips enumeration", "[fe80::1234%7]:7010", "http://[fe80::1234%257]:7010/", "points to this Go listener", 0},
		{"wildcard loopback skips enumeration", "[::]:7010", "http://[::1]:7010/", "points to this Go listener", 0},
		{"different IP skips enumeration", "[fe80::1234%7]:7010", "http://[fe80::5678%257suffix]:7010/", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			discoveryErr := errors.New("synthetic NIC enumeration failure")
			discover := func() ([]proxyInterface, error) {
				calls++
				return nil, discoveryErr
			}
			err := validateProxyTargetWithInterfaces(tc.listen, tc.backend, discover)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("validation must remain allowed: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("validation error=%v, want error containing %q", err, tc.wantErr)
			}
			if calls != tc.wantCalls {
				t.Fatalf("NIC enumeration calls=%d, want %d", calls, tc.wantCalls)
			}
			if tc.wantCalls > 0 && !errors.Is(err, discoveryErr) {
				t.Fatalf("NIC enumeration failure must precede any tentative self verdict and remain wrapped: %v", err)
			}
		})
	}
}

// 検証エラーには URL 自体を出さず、合成した userinfo と token が漏れないことを確認する。
func TestProxyTargetErrorsDoNotExposeCredentials(t *testing.T) {
	const username = "synthetic-proxy-user"
	const password = "synthetic-proxy-password"
	const token = "synthetic-proxy-token"
	cases := []struct {
		name    string
		listen  string
		backend string
		wantErr string
	}{
		{"self proxy", "127.0.0.1:7010", "http://" + username + ":" + password + "@127.0.0.1:7010/?token=" + token, "points to this Go listener"},
		{"parse error", "127.0.0.1:7010", "http://" + username + ":" + password + "@127.0.0.1:invalid/?token=" + token, "absolute HTTP or HTTPS URL"},
		{"out of range backend", "127.0.0.1:7010", "http://" + username + ":" + password + "@127.0.0.1:65536/?token=" + token, "invalid python backend port"},
		{"unsupported scheme", "127.0.0.1:7010", "ftp://" + username + ":" + password + "@127.0.0.1/?token=" + token, "absolute HTTP or HTTPS URL"},
		{"invalid listener", "not-an-address", "http://" + username + ":" + password + "@127.0.0.1:7010/?token=" + token, "invalid listen address"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProxyTarget(tc.listen, tc.backend)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want error containing %q", err, tc.wantErr)
			}
			for _, credential := range []string{username, password, token, tc.backend} {
				if strings.Contains(err.Error(), credential) {
					t.Fatal("proxy target error exposed synthetic credentials")
				}
			}
		})
	}
}

// -no-proxy は明示 backend より優先され、自己転送 guard を通過して DB に到達する。
// DB を用意しない fixture で必ず終了させ、本物のサーバーや実機にはアクセスしない。
func TestMainNoProxyOverridesExplicitBackend(t *testing.T) {
	const childFlag = "KONOMITV_TEST_NO_PROXY_MAIN"
	if os.Getenv(childFlag) == "1" {
		flag.CommandLine = flag.NewFlagSet("konomitv-go", flag.ExitOnError)
		os.Args = []string{"konomitv-go", "-server-dir", os.Getenv("KONOMITV_TEST_SERVER_DIR"), "-listen", "127.0.0.77:7010", "-python-backend", "http://127.0.0.77:7010/", "-no-proxy"}
		main()
		return
	}
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("server:\n  port: 7000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMainNoProxyOverridesExplicitBackend$")
	command.Env = append(os.Environ(), childFlag+"=1", "KONOMITV_TEST_SERVER_DIR="+serverDir)
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("isolated main did not terminate at database initialization: %s", output)
	}
	if err == nil {
		t.Fatalf("fixture without a database must stop at database initialization: %s", output)
	}
	if strings.Contains(string(output), "Invalid proxy configuration") || strings.Contains(string(output), "points to this Go listener") {
		t.Fatalf("-no-proxy did not override explicit self backend: %s", output)
	}
	if !strings.Contains(string(output), "failed to open database") {
		t.Fatalf("main did not pass proxy validation and reach the isolated database: %s", output)
	}
}

// TestMainRejectsSelfProxyBeforeDatabaseOpen は単なる helper のテストではなく、
// main の起動経路が DB 初期化より先に誤設定を拒否することを子プロセスで検証する。
func TestMainRejectsSelfProxyBeforeDatabaseOpen(t *testing.T) {
	if os.Getenv("KONOMITV_TEST_SELF_PROXY_MAIN") == "1" {
		flag.CommandLine = flag.NewFlagSet("konomitv-go", flag.ExitOnError)
		os.Args = []string{"konomitv-go", "-server-dir", os.Getenv("KONOMITV_TEST_SERVER_DIR"), "-listen", "127.0.0.77:7010", "-python-backend", "http://127.0.0.77:7010/"}
		main()
		return
	}
	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("server:\n  port: 7000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestMainRejectsSelfProxyBeforeDatabaseOpen$")
	command.Env = append(os.Environ(), "KONOMITV_TEST_SELF_PROXY_MAIN=1", "KONOMITV_TEST_SERVER_DIR="+serverDir)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("main must reject self-proxy at startup: %s", output)
	}
	if !strings.Contains(string(output), "python backend points to this Go listener") {
		t.Fatalf("main did not reject self-proxy before database initialization: %s", output)
	}
}
