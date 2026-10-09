package api

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aki0429/KonomiTV/server-go/internal/constants"
)

// dataBroadcastingProxyPrefix はデータ放送ブラウザ (web-bml) のリクエストプロキシ API のパス接頭辞。
// 転送先 URL (http:// や https:// で始まる) をパスに含むため、"//" を除去する ServeMux の
// パスクリーニングを通すことができない。そのため ServeMux を使わずに直接ディスパッチする。
const dataBroadcastingProxyPrefix = "/api/data-broadcasting/request/"

// dataBroadcastingUserAgent はデータ放送プロキシで送信する User-Agent (server/app/constants.py の API_REQUEST_HEADERS 相当) 。
var dataBroadcastingUserAgent = "KonomiTV/" + constants.Version

// dataBroadcastingAllowedRequestHeaders はクライアントから上流へ引き継ぐリクエストヘッダー。
var dataBroadcastingAllowedRequestHeaders = []string{"if-modified-since", "cache-control"}

// dataBroadcastingAllowedResponseHeaders は上流からクライアントへ引き継ぐレスポンスヘッダー。
// 'expire' は Python 版の実装 (誤記と思われるが挙動を合わせる) に合わせている。
var dataBroadcastingAllowedResponseHeaders = []string{
	"accept-ranges",
	"authentication-info",
	"last-modified",
	"pragma",
	"date",
	"cache-control",
	"age",
	"expire",
	"content-language",
	"content-location",
	"content-type",
}

// dataBroadcastingInternetStatus は schemas.DataBroadcastingInternetStatus と互換のレスポンス。
type dataBroadcastingInternetStatus struct {
	Success                  bool    `json:"success"`
	IPAddress                *string `json:"ip_address"`
	ResponseTimeMilliseconds *int64  `json:"response_time_milliseconds"`
}

// handleDataBroadcastingProxy は /api/data-broadcasting/request/ 以下のプロキシ API を直接処理する。
// server/app/routers/DataBroadcastingRouter.py の BMLBrowserRequestGETProxyAPI / POSTProxyAPI 相当。
func (s *Server) handleDataBroadcastingProxy(w http.ResponseWriter, r *http.Request) {
	requestURL := strings.TrimPrefix(r.URL.Path, dataBroadcastingProxyPrefix)

	switch r.Method {
	case http.MethodGet:
		s.forwardDataBroadcastingRequest(w, r, http.MethodGet, requestURL, "")
	case http.MethodPost:
		// データ放送ブラウザからの x-www-form-urlencoded 形式の値のキー名は Denbun で固定されている
		_ = r.ParseForm()
		s.forwardDataBroadcastingRequest(w, r, http.MethodPost, requestURL, r.FormValue("Denbun"))
	default:
		writeError(w, http.StatusMethodNotAllowed, "Method Not Allowed")
	}
}

// forwardDataBroadcastingRequest は指定された URL へリクエストを送信し、レスポンスをそのまま転送する。
// Python 版と同様に、上流のステータスコードは転送せず常に 200 を返す点に注意。
func (s *Server) forwardDataBroadcastingRequest(w http.ResponseWriter, r *http.Request, method string, requestURL string, denbun string) {
	// URL が HTTP または HTTPS URL かのバリデーション
	if !strings.HasPrefix(requestURL, "http://") && !strings.HasPrefix(requestURL, "https://") {
		s.logger.Error("request URL must be http or https URL", "url", requestURL)
		writeError(w, http.StatusUnprocessableEntity, "Request URL must be http or https URL")
		return
	}
	s.logger.Debug("data broadcasting request", "method", method, "url", requestURL)
	if method == http.MethodPost {
		s.logger.Debug("data broadcasting request body (Denbun)", "denbun", denbun)
	}

	// リクエストを組み立てる
	var body io.Reader
	if method == http.MethodPost {
		// Python 版と同じく Denbun の値は URL エンコードせずにそのまま連結する
		body = strings.NewReader("Denbun=" + denbun)
	}
	request, err := http.NewRequestWithContext(r.Context(), method, requestURL, body)
	if err != nil {
		s.logger.Error("failed to build data broadcasting request", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to request: %v", err))
		return
	}
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Pragma", "no-cache")
	request.Header.Set("User-Agent", dataBroadcastingUserAgent)
	if method == http.MethodGet {
		request.Header.Set("Accept-Language", "ja")
		// クライアントから許可されたヘッダーのみ引き継ぐ
		for _, headerName := range dataBroadcastingAllowedRequestHeaders {
			if value := r.Header.Get(headerName); value != "" {
				request.Header.Set(headerName, value)
			}
		}
	} else {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}

	// データ放送からアクセスされるサイトは HTTPS でも証明書が切れていることが日常茶飯事なので、証明書の検証を行わない
	// タイムアウトはデータ放送の動作を壊さないようにあえて設定しない
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // Python 版と同じく証明書検証を行わない
		},
	}

	// リクエストを送信する
	response, err := client.Do(request)
	if err != nil {
		s.logger.Error("failed to request data broadcasting URL", "error", err)
		writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to request: %v", err))
		return
	}
	defer func() { _ = response.Body.Close() }()

	// 上流のレスポンスヘッダーのうち、転送する必要があるものだけを引き継ぐ
	allowedHeaders := make(map[string]bool, len(dataBroadcastingAllowedResponseHeaders))
	for _, headerName := range dataBroadcastingAllowedResponseHeaders {
		allowedHeaders[headerName] = true
	}
	for headerName, values := range response.Header {
		if allowedHeaders[strings.ToLower(headerName)] {
			for _, value := range values {
				w.Header().Add(headerName, value)
			}
		}
	}

	// Python 版の StreamingResponse は上流のステータスコードを引き継がず常に 200 を返す
	w.WriteHeader(http.StatusOK)
	if _, err := io.Copy(w, response.Body); err != nil {
		s.logger.Debug("data broadcasting stream was interrupted", "error", err)
	}
}

// handleDataBroadcastingInternetStatus は GET /api/data-broadcasting/internet-status を処理する。
// server/app/routers/DataBroadcastingRouter.py の BMLBrowserInternetStatusAPI 相当。
func (s *Server) handleDataBroadcastingInternetStatus(w http.ResponseWriter, r *http.Request) {
	// Python のシグネチャは destination (必須 str) / is_icmp (bool, 既定 false) /
	// timeout_milliseconds (int, 既定 3000) の宣言順で、FastAPI はこの順に検証して
	// 最初の失敗で打ち切らず全エラーを 1 つの配列 ({"detail": [...]}) にまとめて 422 で返す。
	// Go 版も同じ順序・同じエラー型で再現する。
	v := newFastAPIValidation(r)

	// destination は Python 側の型が str なので空文字も有効値。欠落と空文字を区別する
	destination := v.queryString("destination")

	// is_icmp は既定値つき。値がある場合のみ Pydantic と同じ受理規則 (searchJSONBool) で
	// 解析し、解析できない場合は bool_parsing エラーを積む。queryString と同じく
	// 同名パラメータが複数ある場合は最後の値を採用する。
	isICMP := false
	if values, ok := v.query["is_icmp"]; ok {
		raw := lastQueryValue(values)
		quoted, _ := json.Marshal(raw) // Pydantic と同じく文字列として解釈させる
		parsed, err := searchJSONBool(quoted)
		if err != nil {
			v.details = append(v.details, validationDetail{
				Type:  "bool_parsing",
				Loc:   []any{"query", "is_icmp"},
				Msg:   "Input should be a valid boolean, unable to interpret input",
				Input: raw,
			})
		} else {
			isICMP = parsed
		}
	}

	// timeout_milliseconds は既定値つき。値がある場合のみ queryInt で検証する
	// (queryInt は int64 を超える値でも MaxInt64 へ寄せて受理する)
	timeoutMilliseconds := int64(3000)
	if _, ok := v.query["timeout_milliseconds"]; ok {
		timeoutMilliseconds = v.queryInt("timeout_milliseconds")
	}

	// 検証エラーがあれば FastAPI 互換の 422 を返す
	if v.writeIfInvalid(w) {
		return
	}

	// timeout_milliseconds を time.Duration へ変換する (0 以下・超過値でも panic しない)
	timeout := dataBroadcastingTimeout(timeoutMilliseconds)

	// 応答時間を計測する
	var (
		responseTime time.Duration
		success      bool
	)
	switch {
	case timeout <= 0:
		// timeout_milliseconds <= 0 のとき、Python の asyncio.wait_for は接続を待たずに
		// 即座にタイムアウトさせ success=false を返す (ping3 も応答なし扱い) 。
		// Go の 0 は「タイムアウト無し」を意味するため、接続自体を行わず失敗として扱う。
		success = false
	case isICMP:
		responseTime, success = pingHost(r.Context(), destination, timeout)
	default:
		responseTime, success = dialHost(destination, timeout)
	}

	// 成功した場合のみ、接続先の IP アドレスを解決する
	var ipAddress *string
	if success {
		if resolved := resolveIPv4(r.Context(), destination); resolved != "" {
			ipAddress = &resolved
		}
	}

	var responseTimeMilliseconds *int64
	if success {
		milliseconds := responseTime.Milliseconds()
		responseTimeMilliseconds = &milliseconds
	}

	writeJSON(w, http.StatusOK, dataBroadcastingInternetStatus{
		Success:                  success,
		IPAddress:                ipAddress,
		ResponseTimeMilliseconds: responseTimeMilliseconds,
	})
}

// dataBroadcastingTimeout は timeout_milliseconds (ミリ秒) を time.Duration へ変換する。
// 0 以下は 0 (即時タイムアウト扱い) 、過大な値は 24 時間へ丸める。これにより
// time.Duration の乗算オーバーフローや、それを用いたコンテキストの意図しない即時期限切れを防ぐ。
func dataBroadcastingTimeout(milliseconds int64) time.Duration {
	// 24 時間をミリ秒に換算した上限。Python の int は任意精度だが実用上この上限で十分
	const maximumMilliseconds = int64(24 * 60 * 60 * 1000)
	if milliseconds <= 0 {
		return 0
	}
	if milliseconds > maximumMilliseconds {
		return 24 * time.Hour
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// dialHost は destination:80 への TCP 接続にかかった時間を計測する。
// Python 版の asyncio.open_connection(destination, 80) 相当。
func dialHost(destination string, timeout time.Duration) (time.Duration, bool) {
	start := time.Now()
	connection, err := net.DialTimeout("tcp", net.JoinHostPort(destination, "80"), timeout)
	if err != nil {
		return 0, false
	}
	_ = connection.Close()
	return time.Since(start), true
}

// pingHost は ICMP Echo Request を送信して応答時間を計測する。
//
// Python 版は ping3 ライブラリで ICMP パケットを送る。Go の標準ライブラリには非特権 ICMP
// (Linux の ping socket = SOCK_DGRAM/IPPROTO_ICMP) をそのまま扱う API が無いため、ここでは
// raw socket (net.DialIP) を使う。raw socket の作成には root 権限 (Linux の CAP_NET_RAW /
// Windows の管理者権限) が必要で、非特権 ICMP を利用できない環境では作成が権限エラーで失敗する。
// その場合は ping コマンドなどの特権ヘルパーへフォールバックせず success=false を返す。
// 成功していないのに success=true を返すとデータ放送ブラウザの接続判定を誤らせるため、
// 可用性より判定の正しさを優先する。
func pingHost(ctx context.Context, destination string, timeout time.Duration) (time.Duration, bool) {
	// raw socket の宛先となる IPv4 アドレスを解決する。net.DialIP は IP アドレスしか
	// 受け付けないため、ホスト名はここで解決する。解決できなければ成功にはできない。
	ip := net.ParseIP(resolveIPv4(ctx, destination))
	if ip == nil || ip.To4() == nil {
		return 0, false
	}

	// raw ICMP socket を作成する。権限が無い環境ではここでエラーになり success=false となる
	connection, err := net.DialIP("ip4:icmp", nil, &net.IPAddr{IP: ip.To4()})
	if err != nil {
		return 0, false
	}
	defer func() { _ = connection.Close() }()

	// Python 版は ping3 に秒単位の整数 (int(timeout_milliseconds / 1000)) を渡すため、
	// 1 秒未満は 0 に切り捨てられ、待たずに失敗する。同じ挙動に合わせて秒へ切り捨てる。
	seconds := int64(timeout / time.Second)
	if seconds < 1 {
		return 0, false
	}
	// タイムアウトを過ぎると読み取りがエラーになり success=false となる
	if err := connection.SetDeadline(time.Now().Add(time.Duration(seconds) * time.Second)); err != nil {
		return 0, false
	}

	// 識別子はプロセス ID から作り、応答の照合に使う (ping3 と同様)
	identifier := uint16(os.Getpid() & 0xffff)
	start := time.Now()
	if _, err := connection.Write(buildICMPEchoRequest(identifier, 1)); err != nil {
		return 0, false
	}

	// Echo Reply が届くまで読み続ける。deadline を過ぎると Read がエラーになる
	buffer := make([]byte, 1500)
	for {
		n, err := connection.Read(buffer)
		if err != nil {
			return 0, false
		}
		reply := buffer[:n]
		// raw socket の読み取りには IPv4 ヘッダが含まれるため、ICMP ヘッダの位置を求める
		if offset := icmpHeaderOffset(reply); offset > 0 {
			reply = reply[offset:]
		}
		// Echo Reply (type 0, code 0) かつ識別子が一致するものだけを応答とみなす
		if len(reply) >= 8 && reply[0] == 0 && reply[1] == 0 && binary.BigEndian.Uint16(reply[4:6]) == identifier {
			return time.Since(start), true
		}
		// 無関係なパケットは読み飛ばして deadline まで待つ
	}
}

// buildICMPEchoRequest は ICMPv4 Echo Request パケットを組み立てる。
// ヘッダのチェックサムを計算して埋め、ペイロードは 0 埋めのままとする。
func buildICMPEchoRequest(identifier uint16, sequence uint16) []byte {
	packet := make([]byte, 8+16) // 8 バイトの ICMP ヘッダ + 16 バイトのペイロード
	packet[0] = 8                // type = Echo Request
	packet[1] = 0                // code = 0
	binary.BigEndian.PutUint16(packet[4:6], identifier)
	binary.BigEndian.PutUint16(packet[6:8], sequence)
	binary.BigEndian.PutUint16(packet[2:4], icmpChecksum(packet))
	return packet
}

// icmpChecksum は RFC 1071 の 1 の補数チェックサムを計算する。
func icmpChecksum(packet []byte) uint16 {
	var sum uint32
	for index := 0; index+1 < len(packet); index += 2 {
		sum += uint32(packet[index])<<8 | uint32(packet[index+1])
	}
	if len(packet)%2 == 1 {
		sum += uint32(packet[len(packet)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// icmpHeaderOffset は raw socket が返したパケットから ICMP ヘッダの開始位置を求める。
// IPv4 ヘッダ (バージョン 4 かつプロトコル ICMP) が付いている場合はその長さを返し、
// 付いていない場合は 0 を返す。
func icmpHeaderOffset(packet []byte) int {
	if len(packet) >= 20 && packet[0]>>4 == 4 && packet[9] == 1 {
		headerLength := int(packet[0]&0x0f) * 4
		if headerLength >= 20 && headerLength <= len(packet) {
			return headerLength
		}
	}
	return 0
}

// resolveIPv4 はホスト名を IPv4 アドレスに解決する。
func resolveIPv4(ctx context.Context, destination string) string {
	addresses, err := net.DefaultResolver.LookupIP(ctx, "ip4", destination)
	if err != nil || len(addresses) == 0 {
		return ""
	}
	return addresses[0].String()
}

// parseBoolQuery は FastAPI の bool クエリパラメーターと同じ値を受け付ける。
// IPTV ルーター (iptv.go) からも利用される共通ヘルパー。
func parseBoolQuery(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}
