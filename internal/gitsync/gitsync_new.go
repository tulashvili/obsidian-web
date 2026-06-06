// Package gitsync синхронизирует зашифрованное хранилище с удалённым git.
//
// Все git-операции выполняются в tmpfs-каталоге (RAM): туда восстанавливается
// .git из зашифрованного repo.enc, материализуется рабочее дерево из store,
// выполняется commit/pull/push, после чего результат заново шифруется в store,
// а tmpfs-каталог затирается. На постоянном диске plaintext не остаётся.
package gitsync

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"obsidiansecure/internal/config"
	"obsidiansecure/internal/store"
)

// Progress — колбэк для отправки строк прогресса в UI.
type Progress func(msg string)

type Syncer struct {
	cfg config.Config
	st  *store.Store
	mu  sync.Mutex // не допускаем параллельных синхронизаций
}

func New(cfg config.Config, st *store.Store) *Syncer {
	return &Syncer{cfg: cfg, st: st}
}

// Result — итог синхронизации для отображения в UI.
type Result struct {
	Cloned     bool   `json:"cloned"`
	Committed  bool   `json:"committed"`
	Pushed     bool   `json:"pushed"`
	FilesCount int    `json:"filesCount"`
	Message    string `json:"message"`
}

// Sync выполняет полный цикл: применить локальные правки -> commit -> pull ->
// push -> пересобрать хранилище. Безопасен к параллельным вызовам.
// progress вызывается со строками статуса; может быть nil.
func (s *Syncer) Sync(ctx context.Context, progress Progress) (*Result, error) {
	if progress == nil {
		progress = func(string) {}
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	work := filepath.Join(s.cfg.WorkDir, "repo")
	progress("Подготовка рабочего каталога…")
	if err := os.RemoveAll(work); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	res := &Result{}
	hasRepo := s.repoBlobExists()

	if hasRepo {
		progress("Восстановление .git из хранилища…")
		if err := s.restoreGitDir(work); err != nil {
			return nil, fmt.Errorf("восстановление .git: %w", err)
		}
		progress("Материализация файлов в RAM…")
		if err := s.st.Materialize(work); err != nil {
			return nil, fmt.Errorf("материализация рабочего дерева: %w", err)
		}
	} else {
		if s.cfg.RepoURL == "" {
			return nil, fmt.Errorf("репозиторий не настроен: задайте OSA_REPO_URL")
		}
		progress("Клонирование репозитория…")
		if err := s.run(ctx, work, "clone", s.authURL(), "."); err != nil {
			return nil, fmt.Errorf("clone: %w", err)
		}
		res.Cloned = true
	}

	// Переключаемся на нужную ветку; если её нет — создаём.
	if s.cfg.RepoBranch != "" {
		if err := s.run(ctx, work, "checkout", s.cfg.RepoBranch); err != nil {
			progress(fmt.Sprintf("Ветка «%s» не найдена — создаю…", s.cfg.RepoBranch))
			if err2 := s.run(ctx, work, "checkout", "-b", s.cfg.RepoBranch); err2 != nil {
				return nil, fmt.Errorf("git checkout -b %s: %w", s.cfg.RepoBranch, err2)
			}
		}
	}

	// Коммитим локальные правки, если есть.
	if !res.Cloned {
		progress("Проверка локальных изменений…")
		if err := s.run(ctx, work, "add", "-A"); err != nil {
			return nil, fmt.Errorf("git add: %w", err)
		}
		if s.hasStagedChanges(ctx, work) {
			progress("Создание коммита с локальными правками…")
			s.configIdentity(ctx, work)
			msg := "Изменения из obsidian-secure " + time.Now().Format("2006-01-02 15:04")
			if err := s.run(ctx, work, "commit", "-m", msg); err != nil {
				return nil, fmt.Errorf("git commit: %w", err)
			}
			res.Committed = true
		}
	}

	// Подтягиваем удалённые изменения.
	branchNew := false
	if s.cfg.RepoURL != "" {
		progress("Получение изменений с удалённого (git pull)…")
		if err := s.run(ctx, work, "pull", "--rebase", s.authURL(), s.cfg.RepoBranch); err != nil {
			errStr := err.Error()
			if strings.Contains(errStr, "couldn't find remote ref") ||
				strings.Contains(errStr, "unknown revision") {
				// Ветки нет на remote — будем пушить новую.
				progress(fmt.Sprintf("Ветка «%s» новая, будет создана на remote…", s.cfg.RepoBranch))
				branchNew = true
			} else {
				return nil, fmt.Errorf("git pull: %w (возможен конфликт — разрешите вручную)", err)
			}
		}
	}

	// Пушим если есть что отправить или ветка новая.
	if (res.Committed || branchNew) && s.cfg.RepoURL != "" {
		progress("Отправка изменений на remote (git push)…")
		pushArgs := []string{"push", s.authURL(), s.cfg.RepoBranch}
		if branchNew {
			pushArgs = []string{"push", "--set-upstream", s.authURL(), s.cfg.RepoBranch}
		}
		if err := s.run(ctx, work, pushArgs...); err != nil {
			return nil, fmt.Errorf("git push: %w", err)
		}
		res.Pushed = true
	}

	// Пересобираем зашифрованное хранилище из рабочего дерева.
	progress("Обновление зашифрованного хранилища…")
	paths, err := listTracked(ctx, work)
	if err != nil {
		return nil, err
	}
	if err := s.st.ReplaceAll(work, paths); err != nil {
		return nil, fmt.Errorf("обновление хранилища: %w", err)
	}
	res.FilesCount = len(paths)

	progress("Сохранение .git в хранилище…")
	if err := s.saveGitDir(work); err != nil {
		return nil, fmt.Errorf("сохранение .git: %w", err)
	}

	res.Message = "Синхронизация завершена"
	return res, nil
}

func (s *Syncer) repoBlobExists() bool {
	_, err := os.Stat(s.st.RepoBlobPath())
	return err == nil
}

// authURL подставляет учётные данные в URL для HTTPS-репозиториев.
func (s *Syncer) authURL() string {
	u := s.cfg.RepoURL
	if s.cfg.GitToken != "" && strings.HasPrefix(u, "https://") {
		rest := strings.TrimPrefix(u, "https://")
		return "https://" + s.cfg.GitUsername + ":" + s.cfg.GitToken + "@" + rest
	}
	return u
}

func (s *Syncer) configIdentity(ctx context.Context, work string) {
	_ = s.run(ctx, work, "config", "user.email", "app@obsidian-secure.local")
	_ = s.run(ctx, work, "config", "user.name", "obsidian-secure")
}

func (s *Syncer) hasStagedChanges(ctx context.Context, work string) bool {
	cmd := s.cmd(ctx, work, "diff", "--cached", "--quiet")
	err := cmd.Run()
	return err != nil
}

func (s *Syncer) run(ctx context.Context, work string, args ...string) error {
	cmd := s.cmd(ctx, work, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func (s *Syncer) cmd(ctx context.Context, work string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = work
	// StrictHostKeyChecking=accept-new — принимаем ключ хоста при первом
	// подключении (без интерактивного подтверждения внутри контейнера),
	// но отклоняем изменившиеся ключи (защита от MITM).
	sshCmd := "ssh -o StrictHostKeyChecking=accept-new"
	if s.cfg.GitSSHKeyPath != "" {
		sshCmd += " -i " + s.cfg.GitSSHKeyPath
	}
	env := append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_SSH_COMMAND="+sshCmd,
	)
	cmd.Env = env
	return cmd
}

// listTracked возвращает пути всех отслеживаемых git файлов (без .git).
func listTracked(ctx context.Context, work string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "ls-files", "-z")
	cmd.Dir = work
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	parts := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	res := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			res = append(res, p)
		}
	}
	return res, nil
}

// --- Шифрование каталога .git как tar в repo.enc ---

func (s *Syncer) saveGitDir(work string) error {
	gitDir := filepath.Join(work, ".git")
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.Walk(gitDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(work, path)
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(tw, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return s.st.Cipher().EncryptToFile(s.st.RepoBlobPath(), buf.Bytes())
}

func (s *Syncer) restoreGitDir(work string) error {
	blob, err := s.st.Cipher().DecryptFile(s.st.RepoBlobPath())
	if err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		target := filepath.Join(work, filepath.FromSlash(hdr.Name))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			f.Close()
		}
	}
	return nil
}
