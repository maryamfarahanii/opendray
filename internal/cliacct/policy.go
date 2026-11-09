package cliacct

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// policy.go adds an operator-chosen account selection policy on top of
// the upstream least-loaded pick, and prepares token accounts' config
// dirs so they see the host's existing Claude data.

const (
	PolicyBalanced = "balanced" // least-loaded (upstream behaviour)
	PolicyPriority = "priority" // first usable account in Order
)

// Policy decides which enabled account a new or failed-over session gets.
type Policy struct {
	Mode  string   `json:"mode"`
	Order []string `json:"order"`
}

var policyMu sync.Mutex

func policyPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".opendray", "account-policy.json"), nil
}

func loadPolicy() Policy {
	policyMu.Lock()
	defer policyMu.Unlock()
	p := Policy{Mode: PolicyBalanced, Order: []string{}}
	path, err := policyPath()
	if err != nil {
		return p
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return p
	}
	if json.Unmarshal(body, &p) != nil || (p.Mode != PolicyPriority && p.Mode != PolicyBalanced) {
		return Policy{Mode: PolicyBalanced, Order: []string{}}
	}
	if p.Order == nil {
		p.Order = []string{}
	}
	return p
}

func savePolicy(p Policy) error {
	policyMu.Lock()
	defer policyMu.Unlock()
	path, err := policyPath()
	if err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// pickByPriority returns the first usable account in the operator's
// order; accounts missing from Order follow, by name.
func pickByPriority(p Policy, rows []Account, blocked map[string]bool) string {
	usable := func(a Account) bool { return a.Enabled && !blocked[a.ID] }
	byID := make(map[string]Account, len(rows))
	for _, a := range rows {
		byID[a.ID] = a
	}
	for _, id := range p.Order {
		if a, ok := byID[id]; ok && usable(a) {
			return id
		}
	}
	best := ""
	bestName := ""
	for _, a := range rows {
		if usable(a) && (best == "" || a.Name < bestName) {
			best, bestName = a.ID, a.Name
		}
	}
	return best
}

func (h *Handlers) getPolicy(w http.ResponseWriter, _ *http.Request) {
	throttled := []string{}
	if h.svc.throttles != nil {
		throttled = append(throttled, h.svc.throttles.ThrottledIDs()...)
	}
	p := loadPolicy()
	writeJSON(w, http.StatusOK, map[string]any{"mode": p.Mode, "order": p.Order, "throttled": throttled})
}

func (h *Handlers) putPolicy(w http.ResponseWriter, r *http.Request) {
	var p Policy
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if p.Mode != PolicyPriority && p.Mode != PolicyBalanced {
		writeError(w, http.StatusBadRequest, errors.New("mode must be priority or balanced"))
		return
	}
	if p.Order == nil {
		p.Order = []string{}
	}
	if err := savePolicy(p); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// ensureAccountHome makes a token account's CLAUDE_CONFIG_DIR share the
// host's ~/.claude data. Without it the CLI treats the empty dir as a
// first run (onboarding screen) and old conversations are invisible.
// Only missing entries are created, so it is safe on every spawn.
func ensureAccountHome(configDir string) {
	home, err := os.UserHomeDir()
	if err != nil || configDir == "" {
		return
	}
	base := filepath.Join(home, ".claude")
	if filepath.Clean(configDir) == base {
		return
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return
	}
	for _, name := range []string{"projects", "settings.json", "skills", "plugins", "history.jsonl", "file-history"} {
		src, dst := filepath.Join(base, name), filepath.Join(configDir, name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue
		}
		_ = os.Symlink(src, dst)
	}
	cfgPath := filepath.Join(configDir, ".claude.json")
	cfg := map[string]any{}
	if body, err := os.ReadFile(cfgPath); err == nil {
		_ = json.Unmarshal(body, &cfg)
	}
	if done, _ := cfg["hasCompletedOnboarding"].(bool); done {
		return
	}
	global := map[string]any{}
	if body, err := os.ReadFile(filepath.Join(home, ".claude.json")); err == nil {
		_ = json.Unmarshal(body, &global)
	}
	for _, k := range []string{"lastOnboardingVersion", "projects", "mcpServers", "theme", "installMethod"} {
		if v, ok := global[k]; ok {
			cfg[k] = v
		}
	}
	cfg["hasCompletedOnboarding"] = true
	if _, ok := cfg["theme"]; !ok {
		cfg["theme"] = "dark"
	}
	if body, err := json.Marshal(cfg); err == nil {
		_ = os.WriteFile(cfgPath, body, 0o600)
	}
}
