package github

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var defaultAppPermissions = map[string]string{
	"metadata":      permRead,
	"contents":      permRead,
	"pull_requests": permWrite,
	"issues":        permWrite,
	"statuses":      permWrite,
	"deployments":   permWrite,
}

var slugInvalid = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(name string) string {
	return strings.Trim(slugInvalid.ReplaceAllString(strings.ToLower(name), "-"), "-")
}

type newApp struct {
	Name          string            `json:"name"`
	Owner         string            `json:"owner"`
	WebhookURL    string            `json:"webhook_url"`
	WebhookSecret string            `json:"webhook_secret"`
	CallbackURLs  []string          `json:"callback_urls"`
	SetupURL      string            `json:"setup_url"`
	Permissions   map[string]string `json:"permissions"`
	Events        []string          `json:"events"`
}

type appCredentials struct {
	ID             int64             `json:"id"`
	Slug           string            `json:"slug"`
	Name           string            `json:"name"`
	Owner          string            `json:"owner"`
	ClientID       string            `json:"client_id"`
	ClientSecret   string            `json:"client_secret"`
	WebhookURL     string            `json:"webhook_url"`
	WebhookSecret  string            `json:"webhook_secret"`
	CallbackURLs   []string          `json:"callback_urls"`
	SetupURL       string            `json:"setup_url"`
	Permissions    map[string]string `json:"permissions"`
	Events         []string          `json:"events"`
	PrivateKey     string            `json:"private_key"`
	PrivateKeyPath string            `json:"private_key_path"`
}

func (s *Sim) keyPath(slug string) string {
	return filepath.Join(s.cfg.DataDir, "apps", slug+".private-key.pem")
}

func (s *Sim) credentials(v view, a *app) appCredentials {
	owner := ""
	if o := v.w.account(a.OwnerID); o != nil {
		owner = o.Login
	}
	return appCredentials{
		ID: a.ID, Slug: a.Slug, Name: a.Name, Owner: owner, ClientID: a.ClientID, ClientSecret: a.ClientSecret,
		WebhookURL: a.WebhookURL, WebhookSecret: a.WebhookSecret, CallbackURLs: orEmpty(a.CallbackURLs),
		SetupURL: a.SetupURL, Permissions: a.Permissions, Events: orEmpty(a.Events),
		PrivateKey: a.PrivateKeyPEM, PrivateKeyPath: s.keyPath(a.Slug),
	}
}

func (s *Sim) createApp(in newApp) (appCredentials, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return appCredentials{}, fmt.Errorf("generate app key: %w", err)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var out appCredentials
	err = s.update(func(t *tx) error {
		a, err := t.createApp(in, keyPEM)
		if err != nil {
			return err
		}
		out = s.credentials(t.view, a)
		return nil
	})
	if err != nil {
		return appCredentials{}, err
	}
	return out, s.writeKey(out.Slug, keyPEM)
}

func (s *Sim) writeKey(slug, keyPEM string) error {
	path := s.keyPath(slug)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create app key directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(keyPEM), 0o600); err != nil {
		return fmt.Errorf("write app key: %w", err)
	}
	return nil
}

func (t *tx) createApp(in newApp, keyPEM string) (*app, error) {
	slug := slugify(in.Name)
	owner := t.w.accountByLogin(in.Owner)
	switch {
	case slug == "":
		return nil, errValidation("Integration", "name", "invalid")
	case owner == nil || owner.Type == typeBot:
		return nil, fail(http.StatusNotFound, "owner %s does not exist", in.Owner)
	case t.w.appBySlug(slug) != nil || t.w.accountByLogin(slug+"[bot]") != nil:
		return nil, errValidation("Integration", "name", "already_exists")
	}
	if err := checkPermissions(in.Permissions); err != nil {
		return nil, err
	}
	at := now()
	bot := &account{ID: t.w.newID(), Login: slug + "[bot]", Type: typeBot, CreatedAt: at}
	t.w.Accounts = append(t.w.Accounts, bot)
	a := &app{
		ID: t.w.newID(), Slug: slug, Name: in.Name, OwnerID: owner.ID, BotID: bot.ID,
		ClientID: "Iv23li" + rand.Text()[:14], ClientSecret: randomHex(20),
		WebhookURL: in.WebhookURL, WebhookSecret: in.WebhookSecret, HookID: t.w.newID(),
		PrivateKeyPEM: keyPEM, CallbackURLs: in.CallbackURLs, SetupURL: in.SetupURL,
		Permissions: in.Permissions, Events: in.Events, CreatedAt: at, UpdatedAt: at,
	}
	if a.WebhookSecret == "" {
		a.WebhookSecret = randomHex(20)
	}
	if a.Permissions == nil {
		a.Permissions = maps.Clone(defaultAppPermissions)
	}
	if a.Events == nil {
		a.Events = slices.Clone(supportedEvents)
	}
	t.w.Apps = append(t.w.Apps, a)
	t.emitPing(a)
	return a, nil
}

func checkPermissions(perms map[string]string) error {
	for name, level := range perms {
		if name == "" || permLevel(level) == 0 {
			return errValidation("Integration", "permissions", "invalid")
		}
	}
	return nil
}

func (t *tx) installApp(a *app, acct *account, selection string, repoIDs []int64, senderID int64) (*installation, bool, error) {
	if err := t.checkSelection(acct, selection, repoIDs); err != nil {
		return nil, false, err
	}
	if selection == selectionAll {
		repoIDs = nil
	}
	at := now()
	inst := t.w.installationFor(a.ID, acct.ID)
	if inst == nil {
		inst = &installation{
			ID: t.w.newID(), AppID: a.ID, AccountID: acct.ID, RepositorySelection: selection, RepoIDs: repoIDs,
			Permissions: maps.Clone(a.Permissions), Events: slices.Clone(a.Events), CreatedAt: at, UpdatedAt: at,
		}
		t.w.Installations = append(t.w.Installations, inst)
		t.emitInstallation(inst, "created", senderID)
		return inst, true, nil
	}
	before := t.w.installationRepos(inst)
	inst.RepositorySelection, inst.RepoIDs, inst.UpdatedAt = selection, repoIDs, at
	after := t.w.installationRepos(inst)
	t.emitInstallationRepos(inst, missingFrom(after, before), missingFrom(before, after), senderID)
	return inst, false, nil
}

func (t *tx) checkSelection(acct *account, selection string, repoIDs []int64) error {
	switch selection {
	case selectionAll:
		return nil
	case selectionSelected:
	default:
		return errValidation("Installation", "repository_selection", "invalid")
	}
	if len(repoIDs) == 0 {
		return errValidation("Installation", "repositories", "missing")
	}
	for _, id := range repoIDs {
		if r := t.w.repoByID(id); r == nil || r.OwnerID != acct.ID {
			return errValidation("Installation", "repositories", "invalid")
		}
	}
	return nil
}

func missingFrom(rs, other []*repo) []*repo {
	var out []*repo
	for _, r := range rs {
		if !slices.Contains(other, r) {
			out = append(out, r)
		}
	}
	return out
}

func (t *tx) uninstall(inst *installation, senderID int64) {
	t.emitInstallation(inst, "deleted", senderID)
	t.w.Installations = slices.DeleteFunc(t.w.Installations, func(i *installation) bool { return i.ID == inst.ID })
	t.w.revokeTokens(func(tok *token) bool { return tok.InstallationID == inst.ID })
}

func (t *tx) suspend(inst *installation, senderID int64) {
	if inst.SuspendedAt != nil {
		return
	}
	at := now()
	inst.SuspendedAt, inst.SuspendedByID, inst.UpdatedAt = &at, senderID, at
	t.w.revokeTokens(func(tok *token) bool { return tok.InstallationID == inst.ID })
	t.emitInstallation(inst, "suspend", senderID)
}

func (t *tx) unsuspend(inst *installation, senderID int64) {
	if inst.SuspendedAt == nil {
		return
	}
	inst.SuspendedAt, inst.SuspendedByID, inst.UpdatedAt = nil, 0, now()
	t.emitInstallation(inst, "unsuspend", senderID)
}

func (s *Sim) seed() error {
	if s.cfg.DefaultUser != "" {
		err := s.update(func(t *tx) error {
			if t.w.user(s.cfg.DefaultUser) != nil {
				return nil
			}
			_, err := t.createAccount(s.cfg.DefaultUser, s.cfg.DefaultUser, s.cfg.DefaultUser+"@example.com", typeUser)
			return err
		})
		if err != nil {
			return fmt.Errorf("create default user: %w", err)
		}
	}
	if s.cfg.DefaultApp.Name == "" {
		return nil
	}
	if s.cfg.DefaultUser == "" {
		return errors.New("-github-default-app-name needs -github-default-user to own the app")
	}
	return s.seedApp()
}

func (s *Sim) seedApp() error {
	d := s.cfg.DefaultApp
	var keyPEM, slug string
	err := s.update(func(t *tx) error {
		a := t.w.appBySlug(slugify(d.Name))
		if a == nil {
			return nil
		}
		a.WebhookURL, a.CallbackURLs, a.SetupURL, a.UpdatedAt = d.WebhookURL, d.CallbackURLs, d.SetupURL, now()
		if d.WebhookSecret != "" {
			a.WebhookSecret = d.WebhookSecret
		}
		keyPEM, slug = a.PrivateKeyPEM, a.Slug
		return nil
	})
	if err != nil {
		return fmt.Errorf("update default app: %w", err)
	}
	if slug != "" {
		return s.writeKey(slug, keyPEM)
	}
	_, err = s.createApp(newApp{
		Name: d.Name, Owner: s.cfg.DefaultUser, WebhookURL: d.WebhookURL, WebhookSecret: d.WebhookSecret,
		CallbackURLs: d.CallbackURLs, SetupURL: d.SetupURL,
	})
	if err != nil {
		return fmt.Errorf("create default app: %w", err)
	}
	return nil
}
