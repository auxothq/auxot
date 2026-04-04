package agentworker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

const promptWatchDebounce = 350 * time.Millisecond

// promptWatchOps excludes Chmod-only noise (indexers, backups) while still
// catching editor saves (write/rename/create/remove).
const promptWatchOps = fsnotify.Write | fsnotify.Create | fsnotify.Remove | fsnotify.Rename

// isPromptSourceRel reports whether a path relative to the gitagent root is
// baked into BuildSystemPrompt via LoadGitAgent.
func isPromptSourceRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	switch rel {
	case "SOUL.md", "RULES.md", "AGENTS.md", "agent.yaml",
		"memory/MEMORY.md", "memory/context.md",
		"knowledge/index.yaml",
		"hooks/bootstrap.md":
		return true
	}
	return isSkillSKILLRel(rel)
}

func isSkillSKILLRel(rel string) bool {
	rel = filepath.ToSlash(rel)
	const pfx = "skills/"
	if !strings.HasPrefix(rel, pfx) || !strings.HasSuffix(rel, "/SKILL.md") {
		return false
	}
	mid := strings.TrimPrefix(rel, pfx)
	mid = strings.TrimSuffix(mid, "/SKILL.md")
	return mid != "" && !strings.Contains(mid, "/")
}

func absRelToRoot(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return "", err
	}
	return rel, nil
}

func promptWatchEventRelevant(root string, ev fsnotify.Event) bool {
	if !ev.Has(promptWatchOps) {
		return false
	}
	rel, err := absRelToRoot(root, ev.Name)
	if err != nil || strings.HasPrefix(rel, "..") {
		return false
	}
	if isPromptSourceRel(rel) {
		return true
	}
	// New skill directory: watch it and reload once SKILL.md exists.
	if ev.Has(fsnotify.Create) {
		fi, err := os.Stat(ev.Name)
		if err != nil || !fi.IsDir() {
			return false
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == "skills" {
			return true
		}
		if strings.HasPrefix(relSlash, "skills/") && !strings.Contains(strings.TrimPrefix(relSlash, "skills/"), "/") {
			return true
		}
	}
	return false
}

// promptWatchDirs returns directories to register with fsnotify for prompt-related files.
func promptWatchDirs(root string) ([]string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	dirs = append(dirs, absRoot)

	addIfDir := func(rel string) {
		p := filepath.Join(absRoot, rel)
		fi, err := os.Stat(p)
		if err != nil || !fi.IsDir() {
			return
		}
		dirs = append(dirs, p)
	}

	addIfDir("memory")
	addIfDir("knowledge")
	addIfDir("hooks")

	skillsRoot := filepath.Join(absRoot, "skills")
	if fi, err := os.Stat(skillsRoot); err == nil && fi.IsDir() {
		dirs = append(dirs, skillsRoot)
		entries, err := os.ReadDir(skillsRoot)
		if err != nil {
			return dirs, err
		}
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, filepath.Join(skillsRoot, e.Name()))
			}
		}
	}
	return dirs, nil
}

func addPromptWatchPaths(w *fsnotify.Watcher, root string, log *slog.Logger) error {
	dirs, err := promptWatchDirs(root)
	if err != nil {
		return err
	}
	for _, d := range dirs {
		if err := w.Add(d); err != nil {
			return err
		}
	}
	if log != nil {
		log.Debug("prompt file watcher: watching directories", "count", len(dirs))
	}
	return nil
}

// runPromptFileWatcher watches SOUL.md, AGENTS.md, RULES.md, agent.yaml, memory,
// knowledge, hooks, and skill SKILL.md files; debounced reloads mirror reload_policy.
func (w *Worker) runPromptFileWatcher(ctx context.Context) {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		w.logger.Warn("prompt file watcher unavailable", "err", err)
		return
	}
	defer fw.Close()

	if err := addPromptWatchPaths(fw, w.cfg.Dir, w.logger); err != nil {
		w.logger.Warn("prompt file watcher: could not add paths", "err", err)
		return
	}

	var mu sync.Mutex
	var debounce *time.Timer
	schedule := func() {
		mu.Lock()
		defer mu.Unlock()
		if debounce != nil {
			debounce.Stop()
		}
		debounce = time.AfterFunc(promptWatchDebounce, func() {
			if ctx.Err() != nil {
				return
			}
			w.sendContextUpdate()
		})
	}
	defer func() {
		mu.Lock()
		if debounce != nil {
			debounce.Stop()
		}
		mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case err, ok := <-fw.Errors:
			if !ok {
				return
			}
			if err != nil {
				w.logger.Debug("prompt file watcher error", "err", err)
			}
		case ev, ok := <-fw.Events:
			if !ok {
				return
			}
			if !promptWatchEventRelevant(w.cfg.Dir, ev) {
				continue
			}
			if ev.Has(fsnotify.Create) {
				fi, err := os.Stat(ev.Name)
				if err == nil && fi.IsDir() {
					rel, rerr := absRelToRoot(w.cfg.Dir, ev.Name)
					if rerr == nil && strings.HasPrefix(filepath.ToSlash(rel), "skills/") {
						_ = fw.Add(ev.Name)
					}
				}
			}
			schedule()
		}
	}
}
