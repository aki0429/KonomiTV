// Package niconico は Python 版 server/app/routers/NiconicoRouter.py と
// server/app/utils/OAuthCallbackResponse.py のロジックを Go に移植したものを提供する。
//
// 移植元:
//   - server/app/routers/NiconicoRouter.py (NiconicoAuthURLAPI / NiconicoAuthCallbackAPI /
//     NiconicoAccountLogoutAPI)
//   - server/app/utils/OAuthCallbackResponse.py (OAuthCallbackResponse)
//
// ここにはネットワーク通信を伴わない純粋なロジックのみを置く。
// HTTP 通信・DB アクセス・認証は internal/api/niconico.go 側で行う
// (テスト時に httptest へ差し替えられるようにするため) 。
package niconico

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	// OAuthClientID はニコニコ OAuth のクライアント ID 。
	// 移植元: server/app/constants.py の NICONICO_OAUTH_CLIENT_ID
	OAuthClientID = "4JTJdyBZLwMJwaI7"

	// OAuthAuthorizationEndpoint はニコニコ OAuth の認可エンドポイント。
	OAuthAuthorizationEndpoint = "https://oauth.nicovideo.jp/oauth2/authorize"

	// CallbackURL はニコニコ側に事前登録されたコールバック先 URL 。
	// KonomiTV サーバーごとに URL が異なるため、一旦この URL に集約している。
	CallbackURL = "https://app.konomi.tv/api/redirect/niconico"
)

// Scopes は認証 URL に付与するスコープ (NiconicoRouter.py と同一の並び順) 。
var Scopes = []string{
	"offline_access",
	"openid",
	"profile",
	"user.authorities.relives.watch.get",
	"user.authorities.relives.watch.interact",
	"user.premium",
}

// BuildAuthorizationURL はニコニコ OAuth の認証 URL を生成する。
// 移植元: NiconicoRouter.NiconicoAuthURLAPI()
//
// serverNetloc はリクエストの Host ヘッダー (Python 版 request.url.netloc) 、
// clientURL はクライアント (フロントエンド) の URL 、
// userAccessToken はログイン中ユーザーの JWT アクセストークン。
func BuildAuthorizationURL(serverNetloc string, clientURL string, userAccessToken string) string {
	// コールバック後に渡す state 。Python 版 json.dumps(state, ensure_ascii=False) と同じ並び順・
	// 区切り文字 (", " / ": ") で出力する。
	state := `{"server": ` + pythonJSONString("https://"+serverNetloc+"/") +
		`, "client": ` + pythonJSONString(clientURL) +
		`, "user_access_token": ` + pythonJSONString(userAccessToken) + `}`

	// state は URL パラメータとして渡すため Base64 エンコードする
	// 末尾の = はニコニコ側で URL エンコードされることがあるため削除する (Python 版は replace('=', '') で全削除)
	stateBase64 := strings.ReplaceAll(base64.StdEncoding.EncodeToString([]byte(state)), "=", "")

	// 利用するスコープを指定 (Python 版は '%20'.join(...) で空白を %20 にしている)
	scope := strings.Join(Scopes, "%20")

	return fmt.Sprintf(
		"%s?response_type=code&scope=%s&client_id=%s&redirect_uri=%s&state=%s",
		OAuthAuthorizationEndpoint, scope, OAuthClientID, CallbackURL, stateBase64,
	)
}

// RenderOAuthCallbackResponse は OAuth 連携コールバック用の HTML を生成する。
// 移植元: server/app/utils/OAuthCallbackResponse.py
//
// 置換順序は Python 版と同じ ($status$ → $detail$ → $redirect_to$) 。
func RenderOAuthCallbackResponse(statusCode int, detail string, redirectTo string) string {
	html := oauthCallbackHTMLTemplate
	html = strings.ReplaceAll(html, "$status$", strconv.Itoa(statusCode))
	html = strings.ReplaceAll(html, "$detail$", detail)
	html = strings.ReplaceAll(html, "$redirect_to$", redirectTo)
	return html
}

// ParseAuthorizationSchemeParam は Authorization ヘッダーを (scheme, param) に分解する。
// 移植元: fastapi.security.utils.get_authorization_scheme_param()
// ヘッダーが空の場合は ("", "") を返す。
func ParseAuthorizationSchemeParam(authorizationHeader string) (string, string) {
	if authorizationHeader == "" {
		return "", ""
	}
	scheme, parameter, _ := strings.Cut(authorizationHeader, " ")
	return scheme, parameter
}

// ParseIDTokenSubject は id_token (JWT) のペイロードから sub を整数として取り出す。
// 移植元: NiconicoRouter.NiconicoAuthCallbackAPI() の
// int(jwt.get_unverified_claims(id_token).get('sub', 0))
//
// 署名検証は行わない (Python 版と同じく get_unverified_claims() を使う) 。
func ParseIDTokenSubject(idToken string) (int64, error) {
	// Python 版は rsplit('.', 1) → split('.', 1) でペイロード部分を取り出す
	segments := strings.Split(idToken, ".")
	if len(segments) < 2 {
		return 0, errors.New("niconico: id_token is invalid")
	}
	// python-jose の base64url_decode() はパディングを補完する
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return 0, fmt.Errorf("niconico: failed to decode id_token: %w", err)
	}
	var claims struct {
		Subject json.RawMessage `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0, fmt.Errorf("niconico: failed to parse id_token: %w", err)
	}
	// sub がない場合は Python 版と同じく 0 (int(None) ではなく .get('sub', 0)) になる
	if len(claims.Subject) == 0 {
		return 0, nil
	}

	// 文字列 ("12345") の場合
	var asString string
	if err := json.Unmarshal(claims.Subject, &asString); err == nil {
		value, err := strconv.ParseInt(strings.TrimSpace(asString), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("niconico: id_token sub is invalid: %w", err)
		}
		return value, nil
	}
	// 数値 (12345 / 12345.0) の場合
	var asFloat float64
	if err := json.Unmarshal(claims.Subject, &asFloat); err == nil {
		return int64(asFloat), nil
	}
	return 0, errors.New("niconico: id_token sub is invalid")
}

// pythonJSONString は Python の json.dumps(..., ensure_ascii=False) と同じ形式で
// 文字列を JSON 文字列リテラルへ変換する。
//
// Go の encoding/json は "<" ">" "&" や U+2028/U+2029 を \uXXXX にエスケープする点が
// Python と異なるため、state の JSON はこの関数で自前で組み立てている。
func pythonJSONString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if char < 0x20 {
				fmt.Fprintf(&builder, `\u%04x`, char)
			} else {
				builder.WriteRune(char)
			}
		}
	}
	builder.WriteByte('"')
	return builder.String()
}
