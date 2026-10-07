package twitter

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"time"
)

// CookieEncryptionPrefix は暗号化済み Cookie 文字列の接頭辞 (constants.py の
// TWITTER_ACCOUNT_COOKIE_ENCRYPTION_PREFIX と一致) 。
const CookieEncryptionPrefix = "enc:"

// ErrInvalidToken は Fernet トークンの検証に失敗した場合のエラー (Python の InvalidToken 相当) 。
var ErrInvalidToken = errors.New("invalid fernet token")

// fernet は Python の cryptography.fernet.Fernet 互換の暗号化器。
//
// Python 版 KonomiTV は JWT シークレットから
//
//	key = urlsafe_b64encode(sha256(JWT_SECRET_KEY).digest())
//
// を導出し、その鍵で Fernet (AES-128-CBC + HMAC-SHA256) の暗号化を行っている。
// DB に保存された Cookie を相互に読み書きできるようにするため、同じ実装を持つ。
type fernet struct {
	signingKey    []byte
	encryptionKey []byte
}

// NewFernetFromSecret は JWT シークレットから Fernet を生成する。
func NewFernetFromSecret(secret string) *fernet {
	digest := sha256.Sum256([]byte(secret))
	key := base64.URLEncoding.EncodeToString(digest[:])
	return newFernet(key)
}

// newFernet は Fernet の鍵 (urlsafe base64) から暗号化器を生成する。
func newFernet(key string) *fernet {
	decoded, err := base64.URLEncoding.DecodeString(key)
	if err != nil || len(decoded) != 32 {
		// 正しい鍵が渡らなかったことを確実に伝えるため、低下させた鍵で続行せず panic させる
		panic("twitter: invalid fernet key")
	}
	return &fernet{
		signingKey:    decoded[:16],
		encryptionKey: decoded[16:],
	}
}

// Encrypt は平文を Fernet トークン (urlsafe base64) に変換する。
func (fernetInstance *fernet) Encrypt(plainText string) (string, error) {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		return "", fmt.Errorf("failed to generate iv: %w", err)
	}
	return fernetInstance.encryptFromParts([]byte(plainText), time.Now().Unix(), iv)
}

// encryptFromParts は指定された IV / タイムスタンプでトークンを生成する (テスト用) 。
// Python の Fernet._encrypt_from_parts() と同じ構造:
//
//	version(0x80) || timestamp(8, big endian) || IV(16) || ciphertext || HMAC-SHA256
func (fernetInstance *fernet) encryptFromParts(plainText []byte, timestamp int64, iv []byte) (string, error) {
	padded := padding(plainText, aes.BlockSize)
	buffer := &bytes.Buffer{}
	buffer.WriteByte(0x80)
	// timestamp はビッグエンディアンの 8 バイト
	for shift := 56; shift >= 0; shift -= 8 {
		buffer.WriteByte(byte(uint64(timestamp) >> shift))
	}
	buffer.Write(iv)

	block, err := aes.NewCipher(fernetInstance.encryptionKey)
	if err != nil {
		return "", fmt.Errorf("failed to create cipher: %w", err)
	}
	cipherText := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(cipherText, padded)
	buffer.Write(cipherText)

	// HMAC は暗号文までを対象とする
	mac := hmac.New(sha256.New, fernetInstance.signingKey)
	mac.Write(buffer.Bytes())
	buffer.Write(mac.Sum(nil))

	return base64.URLEncoding.EncodeToString(buffer.Bytes()), nil
}

// Decrypt は Fernet トークンを復号する。Python の InvalidToken 相当の場合は ErrInvalidToken を返す。
func (fernetInstance *fernet) Decrypt(token string) ([]byte, error) {
	decoded, err := base64.URLEncoding.DecodeString(token)
	if err != nil {
		return nil, ErrInvalidToken
	}
	if len(decoded) < 1+8+aes.BlockSize+aes.BlockSize+sha256.Size {
		return nil, ErrInvalidToken
	}
	if decoded[0] != 0x80 {
		return nil, ErrInvalidToken
	}
	signature := decoded[len(decoded)-sha256.Size:]
	payload := decoded[:len(decoded)-sha256.Size]

	mac := hmac.New(sha256.New, fernetInstance.signingKey)
	mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return nil, ErrInvalidToken
	}

	iv := payload[9 : 9+aes.BlockSize]
	cipherText := payload[9+aes.BlockSize:]
	if len(cipherText) == 0 || len(cipherText)%aes.BlockSize != 0 {
		return nil, ErrInvalidToken
	}

	block, err := aes.NewCipher(fernetInstance.encryptionKey)
	if err != nil {
		return nil, ErrInvalidToken
	}
	plainText := make([]byte, len(cipherText))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plainText, cipherText)
	unpadded, err := unpad(plainText, aes.BlockSize)
	if err != nil {
		return nil, ErrInvalidToken
	}
	return unpadded, nil
}

// padding は PKCS#7 パディングを付与する。
func padding(data []byte, blockSize int) []byte {
	paddingLength := blockSize - (len(data) % blockSize)
	return append(append([]byte{}, data...), bytes.Repeat([]byte{byte(paddingLength)}, paddingLength)...)
}

// unpad は PKCS#7 パディングを除去する。
func unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, ErrInvalidToken
	}
	paddingLength := int(data[len(data)-1])
	if paddingLength == 0 || paddingLength > blockSize || paddingLength > len(data) {
		return nil, ErrInvalidToken
	}
	for _, value := range data[len(data)-paddingLength:] {
		if int(value) != paddingLength {
			return nil, ErrInvalidToken
		}
	}
	return data[:len(data)-paddingLength], nil
}
