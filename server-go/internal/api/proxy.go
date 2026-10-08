package api

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"unicode/utf8"
)

// proxyOriginalPathKey は、ErrorHandler のログで使う「Director による URL 書き換え前の
// クライアント要求パス」をリクエストの context へ退避するためのキー。
// 書き換え後の r.URL.Path は backendURL の path 成分を含むため、そのままログに出すと
// バックエンド URL の一部が漏えいする。
type proxyOriginalPathKey struct{}

// proxyErrorURL は任意の接続先 URL を診断から除外する。
var proxyErrorURL = regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)

// safeProxyError は外部エラーをログ用の有限かつ秘匿済み診断へ変換する。
// 任意の Error/Unwrap メソッドは実行せず、既知の標準エラーだけを扱う。
func safeProxyError(err error, target *url.URL) string {
	secrets := []string{target.Path, target.RawPath, target.RawQuery, target.Fragment, target.RawFragment}
	if target.User != nil {
		password, _ := target.User.Password()
		secrets = append(secrets, target.User.Username(), password)
	}
	for _, part := range strings.Split(target.Path, "/") {
		secrets = append(secrets, part)
	}
	for key, values := range target.Query() {
		secrets = append(secrets, key)
		secrets = append(secrets, values...)
	}
	// URL parser は非 UTF-8 の percent 復号値も許す。復号後の診断と確実に照合できない
	// 秘密があれば、部分的な秘匿で安全と推測せず、元文字列を持ち込まない固定分類へ閉じる。
	for _, secret := range secrets {
		if !utf8.ValidString(secret) {
			return "unclassified transport error"
		}
	}
	redact := func(text string) string {
		// 不正 escape が併存すると全体の復号は失敗する。復号できなかった文字列は
		// 部分的な置換で安全と推測せず、秘密を含まない固定分類へ閉じる。
		for range 4 {
			decoded, e := url.QueryUnescape(text)
			if e != nil {
				return "unclassified encoded transport error"
			}
			if decoded == text {
				break
			}
			text = decoded
		}
		// 上限後に percent が残る場合は未正規化であり、さらに復号して推測しない。
		if strings.Contains(text, "%") {
			return "unclassified encoded transport error"
		}
		// URL と全秘密成分の出現位置を、互いに重なるものも含めて独立に求め、その和集合を
		// 一回だけ置換する。交替正規表現の leftmost-first では、短い秘密 (例: username)
		// が長い秘密 (例: password) の接頭辞や前方に重なる場合に長い秘密の残りが漏れる。
		// marker は照合後に生成するため再照合されず、連続区間は一つの marker にまとまる。
		covered := make([]bool, len(text))
		mark := func(from, to int) {
			for i := from; i < to; i++ {
				covered[i] = true
			}
		}
		for _, loc := range proxyErrorURL.FindAllStringIndex(text, -1) {
			mark(loc[0], loc[1])
		}
		for _, secret := range secrets {
			if secret == "" {
				continue
			}
			for offset := 0; offset < len(text); {
				found := strings.Index(text[offset:], secret)
				if found < 0 {
					break
				}
				mark(offset+found, offset+found+len(secret))
				offset += found + 1
			}
		}
		var builder strings.Builder
		for i := 0; i < len(text); {
			if !covered[i] {
				builder.WriteByte(text[i])
				i++
				continue
			}
			builder.WriteString("[redacted]")
			for i < len(text) && covered[i] {
				i++
			}
		}
		text = builder.String()
		if len(text) > 2048 {
			// byte 上限はそのままに先頭 byte まで戻す。正常 UTF-8 なら最大3byteで
			// rune 境界に届き、不正な入力でも end の単調減少により有限で終了する。
			end := 2048
			for end > 0 && !utf8.RuneStart(text[end]) {
				end--
			}
			text = text[:end] + " [truncated]"
		}
		return text
	}
	parts := []string{}
	pending := []error{err}
	for visits := 0; len(pending) > 0 && visits < 32; visits++ {
		current := pending[0]
		pending = pending[1:]
		if current == nil {
			continue
		}
		// interface が non-nil でも実体が nil pointer の場合がある。既知型を含め
		// フィールド参照やメソッド呼出より先に検査し、未知型と同じ固定分類へ閉じる。
		value := reflect.ValueOf(current)
		if value.Kind() == reflect.Pointer && value.IsNil() {
			parts = append(parts, "unclassified transport error")
			continue
		}
		// net/url の実構造体から取得し、URL/Op フィールド自体は出力しない。
		if wrapped, ok := current.(*url.Error); ok {
			pending = append(pending, wrapped.Err)
			continue
		}
		typ := reflect.TypeOf(current)
		if typ.Kind() == reflect.Pointer {
			typ = typ.Elem()
		}
		pkg, name := typ.PkgPath(), typ.Name()
		switch {
		case pkg == "errors" && name == "errorString":
			parts = append(parts, current.Error())
		case pkg == "errors" && name == "joinError":
			children := current.(interface{ Unwrap() []error }).Unwrap()
			// 各ノードの展開も有限にし巨大 graph を保持しない。
			if len(children) > 32 {
				children = children[:32]
			}
			pending = append(pending, children...)
		case pkg == "fmt" && (name == "wrapError" || name == "wrapErrors"):
			parts = append(parts, current.Error())
		case pkg == "io/fs" && name == "PathError":
			// os.PathError は io/fs.PathError の alias。内容には触れず従来名で分類する。
			parts = append(parts, "os.PathError")
		case pkg == "net" || pkg == "os" || pkg == "syscall" || pkg == "context":
			// OS・ネットワーク詳細には接続先が含まれ得るため型だけを分類する。
			parts = append(parts, pkg+"."+name)
		default:
			parts = append(parts, "unclassified transport error")
		}
	}
	if len(pending) != 0 {
		parts = append(parts, "error graph truncated")
	}
	if len(parts) == 0 {
		return "transport failure"
	}
	joined := strings.Join(parts, "; ")
	// 秘匿の計算量を有限に保つため、極端に長い診断は内容を持ち込まず固定分類へ閉じる。
	if len(joined) > 64*1024 {
		return "oversized transport error"
	}
	return redact(joined)
}

// newProxy は未移行の API リクエストを Python 版サーバーへ転送するリバースプロキシを生成する。
// backendURL が空の場合は nil を返し、プロキシは無効となる。
func newProxy(backendURL string, logger *slog.Logger) (http.Handler, error) {
	if backendURL == "" {
		return nil, nil
	}
	target, err := url.Parse(backendURL)
	if err != nil {
		return nil, err
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// ストリーミング (TS 配信や SSE) のレスポンスをバッファリングせず即座に転送する
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		// r は Director が書き換えた outreq のことがあり、その URL.Path は backendURL の
		// path 成分を含む。ログには書き換え前に退避したクライアント本来のパスだけを使う
		// (ReverseProxy が書き換え前の req を渡す経路では退避値が無いが、
		//  その URL.Path はクライアント自身のパスなので安全) 。
		path, _ := r.Context().Value(proxyOriginalPathKey{}).(string)
		if path == "" {
			path = r.URL.Path
		}
		// Error() / Unwrap() を任意の実装に対して呼ぶと、秘密漏えいや循環が起こり得る。
		// 標準ライブラリの既知のラッパーだけを有限に走査し、文字列は最終出力前に秘匿する。
		diagnostic := safeProxyError(err, target)
		logger.Error(
			"proxy error",
			slog.String("path", path),
			// backendURL は userinfo/path/query/fragment を含み得るため生のままログに出さない。
			// バックエンドの識別に必要な host:port のみを記録する。
			slog.String("backend", target.Host),
			slog.String("error", diagnostic),
		)
		writeError(w, http.StatusBadGateway, "Bad Gateway")
	}
	// プロキシ経由のリクエストであることをバックエンドに伝える
	originalDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		// Director は URL をバックエンドへ書き換えるため、書き換え前のクライアント要求パスを
		// context へ退避しておく (ErrorHandler のエラーログで使用する) 。
		originalPath := r.URL.Path
		originalDirector(r)
		r.Header.Set("X-Forwarded-Proto", "http")
		// ReverseProxy は RoundTrip にも同じ *http.Request を渡すため、
		// ここで退避した値は ErrorHandler から参照できる。
		*r = *r.WithContext(context.WithValue(r.Context(), proxyOriginalPathKey{}, originalPath))
	}
	// タイムアウトは設定しない (ストリーミング配信があるため) 。
	// バックエンドへ接続できない場合は ErrorHandler が 502 を返す。
	return proxy, nil
}
