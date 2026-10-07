package twitter

import (
	"context"
	"encoding/base64"
	"testing"
)

// TestInvokeScenarios は TwitterGraphQLAPI.invokeGraphQLAPI レベルのエラー分岐の
// 期待値 (invoke_scenarios) と Go 実装の出力を比較する。
func TestInvokeScenarios(t *testing.T) {
	fixture := loadExpectedFixture(t)

	for _, scenario := range fixture.InvokeScenarios {
		scenario := scenario
		t.Run(scenario.Name, func(t *testing.T) {
			backend := newFakeBackend(rawResponseFromFixture(t, scenario.Raw))
			client := newTestClient(t, backend, nil)
			ctx := context.Background()

			var actual any
			switch scenario.Method {
			case "fetchLoggedViewer":
				actual = client.FetchLoggedViewer(ctx)
			case "createRetweet":
				tweetID := stringValue(t, scenario.Kwargs, "tweet_id")
				actual = client.CreateRetweet(ctx, tweetID)
			case "deleteRetweet":
				tweetID := stringValue(t, scenario.Kwargs, "tweet_id")
				actual = client.DeleteRetweet(ctx, tweetID)
			case "favoriteTweet":
				tweetID := stringValue(t, scenario.Kwargs, "tweet_id")
				actual = client.FavoriteTweet(ctx, tweetID)
			case "unfavoriteTweet":
				tweetID := stringValue(t, scenario.Kwargs, "tweet_id")
				actual = client.UnfavoriteTweet(ctx, tweetID)
			default:
				t.Fatalf("unknown method: %s", scenario.Method)
			}

			jsonEqual(t, scenario.Result, actual)
		})
	}
}

// TestErrorMessages は Python 版 ERROR_MESSAGES と同じ対応表かを確認する。
func TestErrorMessages(t *testing.T) {
	// TwitterGraphQLAPI.ERROR_MESSAGES の全エントリ (Python 版と一致必須)
	expected := map[int]string{
		32:  "Twitter アカウントの認証に失敗しました。もう一度連携し直してください。",
		63:  "Twitter アカウントが凍結またはロックされています。",
		64:  "Twitter アカウントが凍結またはロックされています。",
		88:  "Twitter API エンドポイントのレート制限を超えました。",
		89:  "Twitter アクセストークンの有効期限が切れています。",
		99:  "Twitter OAuth クレデンシャルの認証に失敗しました。",
		131: "Twitter でサーバーエラーが発生しています。",
		135: "Twitter アカウントの認証に失敗しました。もう一度連携し直してください。",
		139: "すでにいいねされています。",
		144: "ツイートが非公開かすでに削除されています。",
		179: "フォローしていない非公開アカウントのツイートは表示できません。",
		185: "ツイート数の上限に達しました。",
		186: "ツイートが長過ぎます。",
		187: "ツイートが重複しています。",
		226: "ツイートが自動化されたスパムと判定されました。",
		261: "Twitter API アプリケーションが凍結されています。",
		326: "Twitter アカウントが一時的にロックされています。",
		327: "すでにリツイートされています。",
		328: "このツイートではリツイートは許可されていません。",
		416: "Twitter API アプリケーションが無効化されています。",
	}
	if len(ErrorMessages) != len(expected) {
		t.Fatalf("ErrorMessages length mismatch: got %d, want %d", len(ErrorMessages), len(expected))
	}
	for code, message := range expected {
		if ErrorMessages[code] != message {
			t.Errorf("ErrorMessages[%d] mismatch\n got: %q\nwant: %q", code, ErrorMessages[code], message)
		}
	}
}

// TestJSSnippets は TwitterScrapeBrowser.invokeGraphQLAPI が CDP に渡す JS 式を
// バイト単位で再現できているか確認する。
func TestJSSnippets(t *testing.T) {
	fixture := loadExpectedFixture(t)
	if len(fixture.JSSnippets) == 0 {
		t.Fatal("no js_snippets in fixture")
	}

	for _, snippet := range fixture.JSSnippets {
		snippet := snippet
		t.Run(snippet.Endpoint, func(t *testing.T) {
			variables, ok := parseOrdered(t, snippet.Variables.Raw).(*OrderedMap)
			if !ok {
				t.Fatal("variables is not an object")
			}
			var additionalFlags *OrderedMap
			if len(snippet.AdditionalFlags) > 0 && string(snippet.AdditionalFlags) != "null" {
				parsed, ok := parseOrdered(t, snippet.AdditionalFlags).(*OrderedMap)
				if !ok {
					t.Fatal("additional_flags is not an object")
				}
				additionalFlags = parsed
			}

			actual, err := BuildInvokeGraphQLScript(snippet.Endpoint, variables, additionalFlags)
			if err != nil {
				t.Fatalf("BuildInvokeGraphQLScript failed: %v", err)
			}
			if actual != snippet.JSCode {
				t.Fatalf("JS snippet mismatch\n got: %q\nwant: %q", actual, snippet.JSCode)
			}
		})
	}
}

// TestFernetInteropWithPython は Python cryptography.fernet が生成したトークンを
// Go 実装が復号でき、かつ Go 実装が同じ入力で同じトークンを生成できるかを確認する。
func TestFernetInteropWithPython(t *testing.T) {
	fixture := loadExpectedFixture(t)
	if len(fixture.Fernet) == 0 {
		t.Fatal("no fernet fixtures")
	}

	for index, entry := range fixture.Fernet {
		entry := entry
		t.Run(entry.Secret, func(t *testing.T) {
			// 鍵導出が Python 版 (sha256 -> urlsafe base64) と一致するか
			instance := NewFernetFromSecret(entry.Secret)
			derivedKey := base64.URLEncoding.EncodeToString(sha256Sum(entry.Secret))
			if derivedKey != entry.FernetKey {
				t.Fatalf("derived key mismatch (index %d)\n got: %q\nwant: %q", index, derivedKey, entry.FernetKey)
			}

			// Python が生成したトークンを復号できるか
			decrypted, err := instance.Decrypt(entry.Token)
			if err != nil {
				t.Fatalf("failed to decrypt Python token: %v", err)
			}
			if string(decrypted) != entry.PlainText {
				t.Fatalf("decrypted text mismatch\n got: %q\nwant: %q", string(decrypted), entry.PlainText)
			}

			// 同じ IV / タイムスタンプで Go が生成するトークンが Python と一致するか
			iv, err := base64.URLEncoding.DecodeString(entry.IVBase64)
			if err != nil {
				t.Fatalf("failed to decode iv: %v", err)
			}
			token, err := instance.encryptFromParts([]byte(entry.PlainText), entry.Timestamp, iv)
			if err != nil {
				t.Fatalf("encryptFromParts failed: %v", err)
			}
			if token != entry.Token {
				t.Fatalf("token mismatch\n got: %q\nwant: %q", token, entry.Token)
			}
		})
	}
}

// TestFernetInvalidToken は改ざんされたトークンが復号に失敗することを確認する。
func TestFernetInvalidToken(t *testing.T) {
	instance := NewFernetFromSecret("dummy-jwt-secret-for-fixtures")
	if _, err := instance.Decrypt("not-a-valid-token"); err != ErrInvalidToken {
		t.Fatalf("expected ErrInvalidToken, got %v", err)
	}
}
