package auth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	encMagic     = "m365enc1"
	tokenKeyFile = "token-enc.key"
)

type envelope struct {
	V     string `json:"v"`
	Nonce string `json:"nonce"`
	Data  string `json:"data"`
}

func tokenKeyPath(storePath string) string {
	if p := strings.TrimSpace(os.Getenv("M365_TOKEN_ENC_KEY_FILE")); p != "" {
		return p
	}
	dir := filepath.Dir(storePath)
	if dataDir := strings.TrimSpace(os.Getenv("M365_DATA_DIR")); dataDir != "" {
		dir = dataDir
	}
	return filepath.Join(dir, tokenKeyFile)
}

func loadOrCreateTokenKey(storePath string) ([]byte, error) {
	if env := strings.TrimSpace(os.Getenv("M365_TOKEN_ENC_KEY")); env != "" {
		sum := sha256.Sum256([]byte(env))
		return sum[:], nil
	}
	path := tokenKeyPath(storePath)
	if b, err := os.ReadFile(path); err == nil {
		raw := strings.TrimSpace(string(b))
		if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
			return decoded, nil
		}
		if len(b) == 32 {
			return b, nil
		}
		sum := sha256.Sum256([]byte(raw))
		return sum[:], nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key) + "\n"
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(encoded), 0o600); err != nil {
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(path)
		if err2 := os.Rename(tmp, path); err2 != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
	}
	return key, nil
}

func looksEncrypted(b []byte) bool {
	var env envelope
	if json.Unmarshal(b, &env) != nil {
		return false
	}
	return env.V == encMagic && env.Nonce != "" && env.Data != ""
}

func encryptJSON(plain []byte, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, plain, []byte(encMagic))
	env := envelope{
		V:     encMagic,
		Nonce: base64.StdEncoding.EncodeToString(nonce),
		Data:  base64.StdEncoding.EncodeToString(sealed),
	}
	return json.MarshalIndent(env, "", "  ")
}

func decryptJSON(raw []byte, key []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.V != encMagic {
		return nil, fmt.Errorf("unsupported token envelope %q", env.V)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(env.Data)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, data, []byte(encMagic))
	if err != nil {
		return nil, errors.New("token cache could not be decrypted; check M365_TOKEN_ENC_KEY")
	}
	return plain, nil
}
