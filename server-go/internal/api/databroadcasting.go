package api

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
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

// pingResponseTimePattern は ping コマンドの出力から応答時間 (ms) を抽出する正規表現。
// 日本語環境の "時間=12ms" と英語環境の "time=12.3ms" の両方にマッチする。
var pingResponseTimePattern = regexp.MustCompile(`[=<]([0-9]+(?:\.[0-9]+)?)\s*ms`)

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
func (s *Server) handleDataBroadcastingInternetStatus(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	destination := query.Get("destination")
	if destination == "" {
		writeError(w, http.StatusUnprocessableEntity, "destination is required")
		return
	}
	isICMP := parseBoolQuery(query.Get("is_icmp"))
	timeoutMilliseconds := int64(3000)
	if value := query.Get("timeout_milliseconds"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusUnprocessableEntity, "timeout_milliseconds must be a positive integer")
			return
		}
		timeoutMilliseconds = parsed
	}
	timeout := time.Duration(timeoutMilliseconds) * time.Millisecond

	// 応答時間を計測する
	var (
		responseTime time.Duration
		success      bool
	)
	if isICMP {
		responseTime, success = pingHost(r.Context(), destination, timeout)
	} else {
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

// pingHost はシステムの ping コマンドで応答時間を計測する。
// Python 版は ping3 ライブラリを使うが、Go で raw socket を扱うには権限が必要なため、
// 権限不要で動作するシステムの ping コマンドを利用する。
func pingHost(ctx context.Context, destination string, timeout time.Duration) (time.Duration, bool) {
	var args []string
	switch runtime.GOOS {
	case "windows":
		args = []string{"-n", "1", "-w", strconv.FormatInt(timeout.Milliseconds(), 10), destination}
	case "darwin":
		args = []string{"-c", "1", "-W", strconv.FormatInt(timeout.Milliseconds(), 10), destination}
	default:
		// Linux (iputils / busybox) の -W は秒単位
		seconds := int(timeout.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		args = []string{"-c", "1", "-W", strconv.Itoa(seconds), destination}
	}

	commandContext, cancel := context.WithTimeout(ctx, timeout+2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(commandContext, "ping", args...).CombinedOutput()
	if err != nil {
		return 0, false
	}
	matches := pingResponseTimePattern.FindSubmatch(output)
	if matches == nil {
		return 0, false
	}
	milliseconds, err := strconv.ParseFloat(string(matches[1]), 64)
	if err != nil {
		return 0, false
	}
	return time.Duration(milliseconds * float64(time.Millisecond)), true
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
func parseBoolQuery(value string) bool {
	switch strings.ToLower(value) {
	case "1", "true", "on", "yes":
		return true
	default:
		return false
	}
}
