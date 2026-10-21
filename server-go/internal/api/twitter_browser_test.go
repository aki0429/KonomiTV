package api

import "testing"

func TestTwitterStoredCookieBackendDecryption(t *testing.T) {
	server, _, _ := newTwitterTestServer(t)
	const plain = "# Netscape HTTP Cookie File\n.x.com\tTRUE\t/\tTRUE\t0\tsynthetic\tvalue\n"
	encrypted, err := server.encryptTwitterCookie(plain)
	if err != nil {
		t.Fatal(err)
	}
	for _, stored := range []string{plain, encrypted} {
		account := server.twitterAccountToModel(twitterAccountRecord{AccessTokenSecret: stored})
		decoded, err := account.LoadCookies()
		if err != nil || decoded != plain {
			t.Fatal("stored Cookie could not be restored")
		}
	}
	account := server.twitterAccountToModel(twitterAccountRecord{AccessTokenSecret: "enc:invalid"})
	if _, err := account.LoadCookies(); err == nil {
		t.Fatal("corrupt Cookie accepted")
	}
}
