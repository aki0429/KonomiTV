package api

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// 正常に parse される合成 target と無害な末尾の不正 escape だけで秘匿失敗を再現する。
func TestMinimalPG1EncodedSecretInvalidPercent(t *testing.T) {
	secret := "秘密パスワード"
	target := (&url.URL{Scheme: "http", Host: "minimal-host.invalid:7010", User: url.UserPassword("synthetic-user", secret)}).String()
	parsed, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	encoded := url.QueryEscape(secret)
	got := safeProxyError(errors.New("dial failure: "+encoded+" trailing %zz"), parsed)
	t.Logf("sanitized diagnostic: %q", got)
	if strings.Contains(got, encoded) || strings.Contains(got, secret) {
		t.Error("recoverable password remained in diagnostic")
	}
}

// URL parser が許容する非 UTF-8 の秘密でも、秘匿器の構築で panic せず502を返す。
func TestPG1BoundaryInvalidUTF8Secret(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("invalid UTF-8 target panicked: %v", p)
		}
	}()
	logs, rec := servePG1ProxyError(t, "http://user:%FF@boundary-host.invalid:7010", pg1FailingTransport{err: errors.New("synthetic safe dial failure")})
	if rec.Code != 502 || rec.Body.String() != "{\"detail\":\"Bad Gateway\"}\n" || !strings.Contains(logs, "unclassified transport error") {
		t.Error("invalid target text must fail closed with clean 502")
	}
}

// os.PathError の実型である io/fs alias も、内容や内側メソッドなしで分類する。
func TestPG1BoundaryPathErrorAlias(t *testing.T) {
	target, _ := url.Parse("http://boundary-host.invalid:7010")
	got := safeProxyError(&fs.PathError{Op: "secret-operation", Path: "secret-path", Err: &pg1BoundaryPoison{}}, target)
	if got != "os.PathError" {
		t.Errorf("alias classification lost: %q", got)
	}
}

// 未知エラーは Error/Unwrap の実行自体を禁止し、nil receiver でも同じ分類に閉じる。
type pg1BoundaryPoison struct{}

func (*pg1BoundaryPoison) Error() string { panic("opaque Error called") }
func (*pg1BoundaryPoison) Unwrap() error { panic("opaque Unwrap called") }

// 長い Unicode 診断も byte 上限を守り、rune の途中で切断しない。
func TestPG1BoundaryUTF8Truncation(t *testing.T) {
	target, _ := url.Parse("http://boundary-host.invalid:7010")
	got := safeProxyError(errors.New(strings.Repeat("界", 2000)), target)
	if len(got) > 2060 || !strings.Contains(got, " [truncated]") {
		t.Errorf("length/marker bound lost: %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Error("diagnostic split a UTF-8 rune")
	}
}

// 秘匿済み marker は再置換せず、短い診断の長さは入力の一致数に比例させる。
func TestPG1BoundaryMarkerDoesNotGrow(t *testing.T) {
	target, _ := url.Parse("http://a:b@boundary-host.invalid:7010/c?d=e#f")
	got := safeProxyError(errors.New("synthetic safe dial failure"), target)
	if len(got) > 256 || strings.Count(got, "[redacted]") > 26 {
		t.Errorf("redaction marker recursively grew: length=%d markers=%d", len(got), strings.Count(got, "[redacted]"))
	}
	if strings.Contains(got, "[truncated]") {
		t.Error("short input must not grow to truncation")
	}
}

// 正規の Unicode 五成分を、混在する大文字・小文字の percent 符号化でも検査する。
// 対応層数を超えた表現は秘密を残さず固定分類へ閉じる。
func TestPG1BoundaryPercentPolicy(t *testing.T) {
	secrets := []string{"利用者秘密", "暗証秘密", "経路秘密", "値秘密", "断片秘密"}
	target := (&url.URL{Scheme: "http", Host: "boundary-host.invalid:7010", User: url.UserPassword(secrets[0], secrets[1]), Path: "/" + secrets[2], RawQuery: "token=" + url.QueryEscape(secrets[3]), Fragment: secrets[4]}).String()
	for _, depth := range []int{1, 2, 4, 5, 32} {
		for _, suffix := range []string{"", " trailing %zz"} {
			t.Run(fmt.Sprintf("depth-%d/%q", depth, suffix), func(t *testing.T) {
				encoded := strings.Join(secrets, " ")
				for range depth {
					encoded = url.QueryEscape(encoded)
				}
				// 偶数位置の hex を小文字化して同一文字列の case 混在を作る。
				bytes := []byte(encoded)
				for i := 0; i+2 < len(bytes); i++ {
					if bytes[i] == '%' && i%2 == 0 {
						copy(bytes[i+1:i+3], strings.ToLower(string(bytes[i+1:i+3])))
					}
				}
				logs, rec := servePG1ProxyError(t, target, pg1FailingTransport{err: errors.New("synthetic safe dial failure " + string(bytes) + suffix)})
				if rec.Code != 502 || rec.Body.String() != "{\"detail\":\"Bad Gateway\"}\n" {
					t.Error("unclean 502")
				}
				if !strings.Contains(logs, "path=/api/unimplemented") || !strings.Contains(logs, "boundary-host.invalid:7010") {
					t.Error("request identity lost")
				}
				for _, secret := range secrets {
					if strings.Contains(logs, secret) {
						t.Error("raw component leaked")
					}
					component := secret
					for range depth {
						component = url.QueryEscape(component)
						if strings.Contains(strings.ToLower(logs), strings.ToLower(component)) {
							t.Error("encoded component leaked")
						}
					}
				}
				if strings.Contains(strings.ToLower(logs), strings.ToLower(string(bytes))) {
					t.Error("encoded components leaked")
				}
				if depth <= 4 && suffix == "" && !strings.Contains(logs, "synthetic safe dial failure") {
					t.Error("normal safe diagnostic lost")
				}
				if (depth > 4 || suffix != "") && !strings.Contains(logs, "unclassified encoded transport error") {
					t.Error("unsafe encoding must be classified without retaining source")
				}
			})
		}
	}
}

// 全標準 wrapper と未知の nil pointer も、メソッド非実行の同じ境界を通る。
func TestPG1BoundaryTypedNilTypes(t *testing.T) {
	target, _ := url.Parse("http://boundary-host.invalid:7010")
	for _, sample := range []error{&url.Error{}, &net.OpError{}, errors.New("sample"), fmt.Errorf("outer: %w", errors.New("inner")), fmt.Errorf("%w %w", errors.New("a"), errors.New("b")), errors.Join(errors.New("a")), &pg1CyclicError{}} {
		t.Run(reflect.TypeOf(sample).String(), func(t *testing.T) {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("typed nil panicked: %v", p)
				}
			}()
			got := safeProxyError(reflect.Zero(reflect.TypeOf(sample)).Interface().(error), target)
			if got == "" {
				t.Error("empty diagnostic")
			}
		})
	}
}

// 型付き nil は non-nil error として RoundTrip から渡せる。502 前の panic を HTTP seam で捕捉する。
func TestMinimalPG1TypedNilURLReturns502(t *testing.T) {
	defer func() {
		if p := recover(); p != nil {
			t.Errorf("panic instead of clean 502: %v", p)
		}
	}()
	var typedNil *url.Error
	_, rec := servePG1ProxyError(t, "http://minimal-host.invalid:7010", pg1FailingTransport{err: typedNil})
	if rec.Code != 502 || rec.Body.String() != "{\"detail\":\"Bad Gateway\"}\n" {
		t.Errorf("missing clean 502: %d %q", rec.Code, rec.Body.String())
	}
}
