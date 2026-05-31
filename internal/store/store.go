// Package store — зашифрованное хранилище контента на постоянном томе /data.
//
// Принцип: на диск пишется ТОЛЬКО зашифрованное. Имена файлов не раскрывают
// путей (blob именуется как HMAC от пути с секретом, выведенным из ключа).
// Plaintext отдаётся в RAM по запросу либо материализуется во временный
// tmpfs-каталог только на время git-операций.
package store

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"obsidiansecure/internal/crypto"
)

// FileMeta — метаданные одного файла в хранилище.
type FileMeta struct {
	Path  string `json:"path"`  // путь относительно корня репозитория, нормализованный через "/"
	Size  int64  `json:"size"`  // размер plaintext
	IsDir bool   `json:"-"`     // вычисляется на лету для дерева
}

// indexData — то, что сериализуется в зашифрованный index.enc.
type indexData struct {
	Files    map[string]FileMeta `json:"files"`    // path -> meta
	NameSalt []byte              `json:"nameSalt"` // секрет для HMAC-имён blob'ов
	Version  int                 `json:"version"`
}

type Store struct {
	dir    string // /data
	cipher *crypto.Cipher

	mu    sync.RWMutex
	files map[string]FileMeta
	name  hmacNamer
}

type hmacNamer struct{ salt []byte }

func (h hmacNamer) blobName(path string) string {
	mac := hmac.New(sha256.New, h.salt)
	mac.Write([]byte(path))
	return hex.EncodeToString(mac.Sum(nil)) + ".enc"
}

const (
	metaFile  = "vault-meta.json"
	indexFile = "index.enc"
	repoFile  = "repo.enc"
	blobsDir  = "blobs"
)

// Open инициализирует хранилище в dir с заданным шифром. Если хранилище ещё
// не существует, создаёт пустое.
func Open(dir string, c *crypto.Cipher) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, blobsDir), 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, cipher: c, files: map[string]FileMeta{}}

	idxPath := filepath.Join(dir, indexFile)
	if _, err := os.Stat(idxPath); errors.Is(err, os.ErrNotExist) {
		// Первая инициализация индекса.
		salt := crypto.DefaultKDFParams().Salt // переиспользуем генератор соли
		s.name = hmacNamer{salt: salt}
		if err := s.saveIndexLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}

	if err := s.loadIndex(); err != nil {
		return nil, fmt.Errorf("store: загрузка индекса: %w", err)
	}
	return s, nil
}

func (s *Store) loadIndex() error {
	blob, err := s.cipher.DecryptFile(filepath.Join(s.dir, indexFile))
	if err != nil {
		return err
	}
	var d indexData
	if err := json.Unmarshal(blob, &d); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.files = d.Files
	if s.files == nil {
		s.files = map[string]FileMeta{}
	}
	s.name = hmacNamer{salt: d.NameSalt}
	return nil
}

func (s *Store) saveIndexLocked() error {
	d := indexData{Files: s.files, NameSalt: s.name.salt, Version: 1}
	blob, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return s.cipher.EncryptToFile(filepath.Join(s.dir, indexFile), blob)
}

// List возвращает все файлы, отсортированные по пути.
func (s *Store) List() []FileMeta {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]FileMeta, 0, len(s.files))
	for _, m := range s.files {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Has сообщает, есть ли файл с таким путём.
func (s *Store) Has(path string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.files[normalize(path)]
	return ok
}

// Read расшифровывает и возвращает содержимое файла.
func (s *Store) Read(path string) ([]byte, error) {
	path = normalize(path)
	s.mu.RLock()
	_, ok := s.files[path]
	namer := s.name
	s.mu.RUnlock()
	if !ok {
		return nil, os.ErrNotExist
	}
	return s.cipher.DecryptFile(filepath.Join(s.dir, blobsDir, namer.blobName(path)))
}

// Write шифрует и сохраняет содержимое, обновляя индекс.
func (s *Store) Write(path string, data []byte) error {
	path = normalize(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.cipher.EncryptToFile(filepath.Join(s.dir, blobsDir, s.name.blobName(path)), data); err != nil {
		return err
	}
	s.files[path] = FileMeta{Path: path, Size: int64(len(data))}
	return s.saveIndexLocked()
}

// Delete удаляет файл из хранилища.
func (s *Store) Delete(path string) error {
	path = normalize(path)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.files[path]; !ok {
		return nil
	}
	_ = os.Remove(filepath.Join(s.dir, blobsDir, s.name.blobName(path)))
	delete(s.files, path)
	return s.saveIndexLocked()
}

// RepoBlobPath — путь к зашифрованному tar архиву .git.
func (s *Store) RepoBlobPath() string { return filepath.Join(s.dir, repoFile) }

// Cipher даёт доступ к шифру (для gitsync — шифрование .git архива).
func (s *Store) Cipher() *crypto.Cipher { return s.cipher }

// ReplaceAll полностью пересобирает индекс из набора путей plaintext-файлов,
// читаемых из workDir. Используется после git pull: каждый файл рабочего дерева
// заново шифруется в blob, исчезнувшие файлы удаляются.
func (s *Store) ReplaceAll(workDir string, paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	newFiles := make(map[string]FileMeta, len(paths))
	for _, rel := range paths {
		rel = normalize(rel)
		data, err := os.ReadFile(filepath.Join(workDir, rel))
		if err != nil {
			return fmt.Errorf("store: чтение %s: %w", rel, err)
		}
		if err := s.cipher.EncryptToFile(filepath.Join(s.dir, blobsDir, s.name.blobName(rel)), data); err != nil {
			return err
		}
		newFiles[rel] = FileMeta{Path: rel, Size: int64(len(data))}
	}
	// Удаляем blob'ы пропавших файлов.
	for old := range s.files {
		if _, ok := newFiles[old]; !ok {
			_ = os.Remove(filepath.Join(s.dir, blobsDir, s.name.blobName(old)))
		}
	}
	s.files = newFiles
	return s.saveIndexLocked()
}

// Materialize пишет plaintext всех файлов во временный каталог dst (tmpfs).
// Применяется перед git-операциями, которым нужно реальное рабочее дерево.
func (s *Store) Materialize(dst string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for path := range s.files {
		data, err := s.cipher.DecryptFile(filepath.Join(s.dir, blobsDir, s.name.blobName(path)))
		if err != nil {
			return err
		}
		full := filepath.Join(dst, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(full, data, 0o600); err != nil {
			return err
		}
	}
	return nil
}

// normalize приводит путь к виду с прямыми слэшами без ведущего "./" и "/".
func normalize(p string) string {
	p = filepath.ToSlash(p)
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	return p
}
