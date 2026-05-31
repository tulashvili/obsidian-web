// Package crypto реализует ядро безопасности приложения.
//
// Модель угрозы: злоумышленник получил физический доступ к диску (volume /data).
// Гарантия: без мастер-пароля содержимое /data нечитаемо. Ключ выводится из
// пароля через Argon2id и существует ТОЛЬКО в оперативной памяти процесса —
// на диск он не пишется никогда.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/crypto/argon2"
)

const (
	keyLen   = 32 // AES-256
	saltLen  = 16
	nonceLen = 12 // GCM standard nonce
)

// KDFParams — параметры Argon2id. Не секретны, хранятся рядом с зашифрованными
// данными в открытом виде, чтобы ключ можно было воспроизвести из пароля.
type KDFParams struct {
	Salt    []byte `json:"salt"`
	Time    uint32 `json:"time"`    // число итераций
	Memory  uint32 `json:"memory"`  // в КиБ
	Threads uint8  `json:"threads"` // степень параллелизма
}

// DefaultKDFParams возвращает разумные параметры для интерактивного входа.
// 64 МиБ памяти, 3 прохода — баланс между защитой от перебора и скоростью.
func DefaultKDFParams() KDFParams {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic("crypto: не удалось сгенерировать соль: " + err.Error())
	}
	return KDFParams{
		Salt:    salt,
		Time:    3,
		Memory:  64 * 1024,
		Threads: 4,
	}
}

// Cipher держит выведенный из пароля ключ и предоставляет AEAD-операции.
// Хранится только в RAM на время жизни процесса.
type Cipher struct {
	aead cipher.AEAD
	// verifier — детерминированный отпечаток ключа для проверки пароля,
	// не раскрывающий сам ключ.
	verifier []byte
}

// NewCipher выводит ключ из пароля по заданным параметрам и строит AEAD.
func NewCipher(password []byte, p KDFParams) (*Cipher, error) {
	if len(password) == 0 {
		return nil, errors.New("crypto: пустой пароль")
	}
	key := argon2.IDKey(password, p.Salt, p.Time, p.Memory, p.Threads, keyLen)

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm: %w", err)
	}

	// Верификатор пароля: шифруем фиксированную строку с нулевым nonce
	// ОТДЕЛЬНЫМ выводом ключа, чтобы не переиспользовать боевой ключ под
	// статичный nonce. Берём отдельный «verify» поддомен через argon2 c той же
	// солью, но другим контекстом длины.
	vkey := argon2.IDKey(append([]byte("verify:"), password...), p.Salt, p.Time, p.Memory, p.Threads, keyLen)
	c := &Cipher{aead: aead, verifier: vkey}
	return c, nil
}

// Verifier возвращает отпечаток ключа для сохранения на диск. По нему при
// следующем запуске проверяется правильность введённого пароля без хранения
// самого ключа или пароля.
func (c *Cipher) Verifier() []byte {
	out := make([]byte, len(c.verifier))
	copy(out, c.verifier)
	return out
}

// CheckVerifier сравнивает сохранённый отпечаток с текущим в постоянное время.
func (c *Cipher) CheckVerifier(stored []byte) bool {
	return subtle.ConstantTimeCompare(stored, c.verifier) == 1
}

// Encrypt шифрует plaintext. Формат выхода: nonce(12) || ciphertext+tag.
// Каждый вызов использует свежий случайный nonce.
func (c *Cipher) Encrypt(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, nonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("crypto: nonce: %w", err)
	}
	out := c.aead.Seal(nonce, nonce, plaintext, nil)
	return out, nil
}

// Decrypt расшифровывает данные формата Encrypt. Возвращает ошибку при
// нарушении целостности (неверный ключ или повреждение).
func (c *Cipher) Decrypt(blob []byte) ([]byte, error) {
	if len(blob) < nonceLen {
		return nil, errors.New("crypto: данные короче nonce")
	}
	nonce, ct := blob[:nonceLen], blob[nonceLen:]
	pt, err := c.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: расшифровка не удалась (неверный пароль или повреждение): %w", err)
	}
	return pt, nil
}

// EncryptToFile шифрует данные и атомарно записывает в path.
func (c *Cipher) EncryptToFile(path string, plaintext []byte) error {
	blob, err := c.Encrypt(plaintext)
	if err != nil {
		return err
	}
	return atomicWrite(path, blob)
}

// DecryptFile читает зашифрованный файл и возвращает plaintext.
func (c *Cipher) DecryptFile(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return c.Decrypt(blob)
}

// atomicWrite пишет во временный файл и переименовывает — защита от частичной
// записи при сбое.
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// --- Сохранение/загрузка параметров KDF и верификатора ---

type vaultMeta struct {
	KDF      KDFParams `json:"kdf"`
	Verifier []byte    `json:"verifier"`
	Version  int       `json:"version"`
}

// SaveVaultMeta сохраняет открытые параметры KDF и отпечаток ключа.
func SaveVaultMeta(path string, p KDFParams, verifier []byte) error {
	m := vaultMeta{KDF: p, Verifier: verifier, Version: 1}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// LoadVaultMeta читает параметры KDF и отпечаток. ok=false если файла нет
// (первый запуск — хранилище ещё не инициализировано).
func LoadVaultMeta(path string) (p KDFParams, verifier []byte, ok bool, err error) {
	data, rerr := os.ReadFile(path)
	if errors.Is(rerr, os.ErrNotExist) {
		return KDFParams{}, nil, false, nil
	}
	if rerr != nil {
		return KDFParams{}, nil, false, rerr
	}
	var m vaultMeta
	if err = json.Unmarshal(data, &m); err != nil {
		return KDFParams{}, nil, false, err
	}
	return m.KDF, m.Verifier, true, nil
}

// Wipe затирает байтовый срез нулями (для пароля после использования).
func Wipe(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// reader-helper для будущего стримингового шифрования больших assets.
var _ io.Reader
