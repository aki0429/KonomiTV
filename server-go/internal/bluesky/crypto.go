package bluesky

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ***** Fernet 互換のセッション文字列暗号化 *****

// SessionEncryptionPrefix は暗号化されたセッション文字列であることを示す接頭辞。
const SessionEncryptionPrefix = "enc:"

// ErrInvalidToken は Fernet トークンの復号に失敗したことを表す。
var ErrInvalidToken = errors.New("invalid fernet token")

// Fernet は Python cryptography の Fernet と互換の暗号化を提供する。
// server/app/constants.py の BLUESKY_ACCOUNT_SESSION_FERNET_KEY と同じ鍵を導出する。
type Fernet struct {
	signingKey []byte
	encryptKey []byte
}

// NewFernetWithJWTSecret は jwt_secret.dat の内容から Fernet 鍵を導出する。
// Python 版: base64.urlsafe_b64encode(hashlib.sha256(f'bluesky:{JWT_SECRET_KEY}'.encode()).digest())
func NewFernetWithJWTSecret(jwtSecret string) *Fernet {
	digest := sha256.Sum256([]byte("bluesky:" + jwtSecret))
	return &Fernet{signingKey: digest[:16], encryptKey: digest[16:]}
}

// Encrypt は平文を Fernet トークン (base64url) に暗号化する。
func (f *Fernet) Encrypt(plainText string) (string, error) {
	return f.EncryptAt(plainText, time.Now())
}

// EncryptAt は指定された時刻で Fernet トークンを生成する (テスト用) 。
func (f *Fernet) EncryptAt(plainText string, timestamp time.Time) (string, error) {
	initializationVector := make([]byte, aes.BlockSize)
	if _, err := rand.Read(initializationVector); err != nil {
		return "", fmt.Errorf("failed to generate fernet iv: %w", err)
	}
	return f.encryptWithIV(plainText, timestamp, initializationVector)
}

// EncryptWithIVForTest は IV を固定して Fernet トークンを生成する (Python 実装との比較テスト用) 。
func (f *Fernet) EncryptWithIVForTest(plainText string, timestamp time.Time, initializationVector []byte) (string, error) {
	if len(initializationVector) != aes.BlockSize {
		return "", fmt.Errorf("invalid initialization vector length: %d", len(initializationVector))
	}
	return f.encryptWithIV(plainText, timestamp, initializationVector)
}

// encryptWithIV は Fernet の token 構造 (Version | Timestamp | IV | Ciphertext | HMAC) を組み立てる。
func (f *Fernet) encryptWithIV(plainText string, timestamp time.Time, initializationVector []byte) (string, error) {
	block, err := aes.NewCipher(f.encryptKey)
	if err != nil {
		return "", fmt.Errorf("failed to create aes cipher: %w", err)
	}

	// PKCS7 パディング
	padded := pkcs7Pad([]byte(plainText), aes.BlockSize)

	header := make([]byte, 0, 1+8+len(initializationVector))
	header = append(header, 0x80)
	timestampBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(timestampBytes, uint64(timestamp.Unix()))
	header = append(header, timestampBytes...)
	header = append(header, initializationVector...)

	encrypted := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, initializationVector).CryptBlocks(encrypted, padded)

	payload := append(append([]byte{}, header...), encrypted...)
	signature := hmac.New(sha256.New, f.signingKey)
	signature.Write(payload)

	token := append(append([]byte{}, payload...), signature.Sum(nil)...)
	return base64.URLEncoding.EncodeToString(token), nil
}

// Decrypt は Fernet トークンを復号する。改ざん・不正なトークンの場合は ErrInvalidToken を返す。
func (f *Fernet) Decrypt(token string) (string, error) {
	raw, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		// Python 版も URL-safe base64 の改行なし形式で受け取る
		return "", ErrInvalidToken
	}
	if len(raw) < 1+8+aes.BlockSize+sha256.Size {
		return "", ErrInvalidToken
	}
	if raw[0] != 0x80 {
		return "", ErrInvalidToken
	}

	payload := raw[:len(raw)-sha256.Size]
	expectedSignature := raw[len(raw)-sha256.Size:]

	signature := hmac.New(sha256.New, f.signingKey)
	signature.Write(payload)
	if !hmac.Equal(expectedSignature, signature.Sum(nil)) {
		return "", ErrInvalidToken
	}

	initializationVector := payload[9 : 9+aes.BlockSize]
	cipherText := payload[9+aes.BlockSize:]
	if len(cipherText) == 0 || len(cipherText)%aes.BlockSize != 0 {
		return "", ErrInvalidToken
	}

	block, err := aes.NewCipher(f.encryptKey)
	if err != nil {
		return "", ErrInvalidToken
	}
	decrypted := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, initializationVector).CryptBlocks(decrypted, cipherText)

	unpadded, err := pkcs7Unpad(decrypted, aes.BlockSize)
	if err != nil {
		return "", ErrInvalidToken
	}
	return string(unpadded), nil
}

// EncryptSessionString は Python 版 BlueskyAccount.encryptSessionString() 相当の処理を行う。
func EncryptSessionString(fernet *Fernet, plainText string) (string, error) {
	// 空文字は暗号化不要なのでそのまま返す
	if plainText == "" {
		return "", nil
	}
	encrypted, err := fernet.Encrypt(plainText)
	if err != nil {
		return "", err
	}
	return SessionEncryptionPrefix + encrypted, nil
}

// DecryptSessionString は Python 版 BlueskyAccount.decryptSessionString() 相当の処理を行う。
func DecryptSessionString(fernet *Fernet, sessionString string) (string, error) {
	if sessionString == "" {
		return "", nil
	}
	if !strings.HasPrefix(sessionString, SessionEncryptionPrefix) {
		// 接頭辞が無い場合は平文として扱う
		return sessionString, nil
	}
	return fernet.Decrypt(strings.TrimPrefix(sessionString, SessionEncryptionPrefix))
}

// pkcs7Pad は PKCS#7 パディングを付与する。
func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	padded := make([]byte, len(data)+padding)
	copy(padded, data)
	for index := len(data); index < len(padded); index++ {
		padded[index] = byte(padding)
	}
	return padded
}

// pkcs7Unpad は PKCS#7 パディングを除去する。
func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, ErrInvalidToken
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, ErrInvalidToken
	}
	for index := len(data) - padding; index < len(data); index++ {
		if data[index] != byte(padding) {
			return nil, ErrInvalidToken
		}
	}
	return data[:len(data)-padding], nil
}

// ***** atproto SDK の Session 文字列 *****

// sessionStringSeparator は atproto SDK の Session.encode() と同じ区切り文字。
const sessionStringSeparator = ":::"

// Session は atproto SDK の Session と同等のセッション情報。
type Session struct {
	Handle string
	DID    string
	// AccessJWT はアクセストークン (JWT) 。
	AccessJWT string
	// RefreshJWT はリフレッシュトークン (JWT) 。
	RefreshJWT string
	// PDSEndpoint は PDS のベース URL 。
	PDSEndpoint string
}

// Encode は Session を atproto SDK 互換のセッション文字列へ変換する。
func (s *Session) Encode() string {
	return strings.Join([]string{s.Handle, s.DID, s.AccessJWT, s.RefreshJWT, s.PDSEndpoint}, sessionStringSeparator)
}

// DecodeSessionString は atproto SDK 互換のセッション文字列を Session へ変換する。
// 旧形式 (pds_endpoint なしの 4 フィールド) も受け付ける。
func DecodeSessionString(sessionString string) (*Session, error) {
	fields := strings.Split(sessionString, sessionStringSeparator)
	switch len(fields) {
	case 4:
		return &Session{Handle: fields[0], DID: fields[1], AccessJWT: fields[2], RefreshJWT: fields[3], PDSEndpoint: "https://bsky.social"}, nil
	case 5:
		return &Session{Handle: fields[0], DID: fields[1], AccessJWT: fields[2], RefreshJWT: fields[3], PDSEndpoint: fields[4]}, nil
	default:
		return nil, fmt.Errorf("invalid session string: %d fields", len(fields))
	}
}

// accessTokenExpiresAt はアクセストークン (JWT) の exp を返す。
func accessTokenExpiresAt(accessToken string) (time.Time, error) {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return time.Time{}, errors.New("invalid jwt: not enough segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// パディング付きの base64url で符号化されている場合も受け付ける
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, fmt.Errorf("invalid jwt payload: %w", err)
		}
	}
	var claims struct {
		Expiration *int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, fmt.Errorf("invalid jwt payload: %w", err)
	}
	if claims.Expiration == nil || *claims.Expiration == 0 {
		return time.Time{}, errors.New("jwt does not have exp claim")
	}
	return time.Unix(*claims.Expiration, 0), nil
}
