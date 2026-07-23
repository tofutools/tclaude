package agentd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/zalando/go-keyring"
)

// withTokenTestEnv isolates the operator-token globals (keychain accessors,
// file path, live token) and restores them after the test, so each case runs
// against a clean, hermetic backend with no real keychain / ~/.tclaude writes.
func withTokenTestEnv(t *testing.T) (filePath string) {
	t.Helper()
	prevGet, prevSet, prevPath, prevTok := keychainGet, keychainSet, operatorTokenFilePath, operatorToken
	t.Cleanup(func() {
		keychainGet, keychainSet, operatorTokenFilePath, operatorToken = prevGet, prevSet, prevPath, prevTok
	})
	fp := filepath.Join(t.TempDir(), ".tclaude", "operator_token")
	operatorTokenFilePath = func() string { return fp }
	// Default: keychain entirely unavailable (the WSL/headless case). Cases
	// that exercise the keychain override these.
	keychainGet = func(string, string) (string, error) { return "", errors.New("no keychain backend") }
	keychainSet = func(string, string, string) error { return errors.New("no keychain backend") }
	return fp
}

func TestShouldPersistOperatorToken(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		cfg  *config.Config
		want bool
	}{
		{"default off", false, &config.Config{}, false},
		{"flag on", true, &config.Config{}, true},
		{"config on", false, &config.Config{Agent: &config.AgentConfig{PersistOperatorToken: true}}, true},
		{"flag OR config", true, &config.Config{Agent: &config.AgentConfig{PersistOperatorToken: false}}, true},
		{"nil cfg", false, nil, false},
		{"nil agent", false, &config.Config{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPersistOperatorToken(tc.flag, tc.cfg); got != tc.want {
				t.Fatalf("shouldPersistOperatorToken(%v, %+v) = %v, want %v", tc.flag, tc.cfg, got, tc.want)
			}
		})
	}
}

func TestShouldPersistOperatorTokenKeychain(t *testing.T) {
	cases := []struct {
		name string
		flag bool
		cfg  *config.Config
		want bool
	}{
		{"default off", false, &config.Config{}, false},
		{"flag on", true, &config.Config{}, true},
		{"config on", false, &config.Config{Agent: &config.AgentConfig{PersistOperatorTokenKeychain: true}}, true},
		{"flag OR config", true, &config.Config{Agent: &config.AgentConfig{PersistOperatorTokenKeychain: false}}, true},
		{"ordinary persistence does not select keychain", false, &config.Config{Agent: &config.AgentConfig{PersistOperatorToken: true}}, false},
		{"nil cfg", false, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldPersistOperatorTokenKeychain(tc.flag, tc.cfg); got != tc.want {
				t.Fatalf("shouldPersistOperatorTokenKeychain(%v, %+v) = %v, want %v", tc.flag, tc.cfg, got, tc.want)
			}
		})
	}
}

func TestResolveOperatorTokenPersistence(t *testing.T) {
	cases := []struct {
		name                      string
		persistFlag, keychainFlag bool
		cfg                       *config.Config
		wantPersist, wantKeychain bool
	}{
		{name: "default ephemeral", cfg: &config.Config{}},
		{name: "file flag", persistFlag: true, cfg: &config.Config{}, wantPersist: true},
		{name: "file config", cfg: &config.Config{Agent: &config.AgentConfig{PersistOperatorToken: true}}, wantPersist: true},
		{name: "keychain flag implies persistence", keychainFlag: true, cfg: &config.Config{}, wantPersist: true, wantKeychain: true},
		{name: "keychain config implies persistence", cfg: &config.Config{Agent: &config.AgentConfig{PersistOperatorTokenKeychain: true}}, wantPersist: true, wantKeychain: true},
		{
			name:        "keychain wins when both selected",
			persistFlag: true,
			cfg: &config.Config{Agent: &config.AgentConfig{
				PersistOperatorToken: true, PersistOperatorTokenKeychain: true,
			}},
			wantPersist: true, wantKeychain: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			persist, keychain := resolveOperatorTokenPersistence(tc.persistFlag, tc.keychainFlag, tc.cfg)
			if persist != tc.wantPersist || keychain != tc.wantKeychain {
				t.Fatalf("resolveOperatorTokenPersistence() = (%v, %v), want (%v, %v)",
					persist, keychain, tc.wantPersist, tc.wantKeychain)
			}
		})
	}
}

func TestResolveOperatorToken_EphemeralByDefault(t *testing.T) {
	withTokenTestEnv(t)
	tok, src := resolveOperatorToken(false, false)
	if src.kind != tokenSourceEphemeral {
		t.Fatalf("source = %q, want ephemeral", src.kind)
	}
	if !strings.HasPrefix(tok, humanTokenPrefix) {
		t.Fatalf("token %q missing prefix %q", tok, humanTokenPrefix)
	}
	if currentOperatorToken() != tok {
		t.Fatalf("live token not installed: got %q want %q", currentOperatorToken(), tok)
	}
	// Ephemeral must not touch the file backend.
	if _, err := os.Stat(operatorTokenFilePath()); !os.IsNotExist(err) {
		t.Fatalf("ephemeral path wrote a token file (stat err = %v)", err)
	}
}

func TestResolveOperatorToken_FileBackedByDefault(t *testing.T) {
	fp := withTokenTestEnv(t)
	keychainGet = func(string, string) (string, error) {
		t.Fatal("default file persistence must not inspect the keychain")
		return "", nil
	}
	keychainSet = func(string, string, string) error {
		t.Fatal("default file persistence must not write the keychain")
		return nil
	}

	tok, src := resolveOperatorToken(true, false)
	if src.kind != tokenSourceFile {
		t.Fatalf("source = %q, want file", src.kind)
	}
	if src.path != fp {
		t.Fatalf("source path = %q, want %q", src.path, fp)
	}
	if !strings.HasPrefix(tok, humanTokenPrefix) {
		t.Fatalf("token %q missing prefix %q", tok, humanTokenPrefix)
	}
	if currentOperatorToken() != tok {
		t.Fatalf("live token mismatch")
	}

	// File written 0600.
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("token file perm = %o, want 0600", perm)
	}

	// Persistence: a second resolve (a "restart") returns the SAME token.
	tok2, src2 := resolveOperatorToken(true, false)
	if tok2 != tok {
		t.Fatalf("token changed across restart: %q -> %q", tok, tok2)
	}
	if src2.kind != tokenSourceFile {
		t.Fatalf("second source = %q, want file", src2.kind)
	}
}

func TestResolveOperatorToken_KeychainOptIn(t *testing.T) {
	withTokenTestEnv(t)
	const stored = "tclo_explicit_keychain_value"
	keychainGet = func(string, string) (string, error) { return stored, nil }

	tok, src := resolveOperatorToken(true, true)
	if tok != stored || src.kind != tokenSourceKeychain {
		t.Fatalf("resolveOperatorToken(keychain) = (%q, %q), want (%q, keychain)", tok, src.kind, stored)
	}
	if currentOperatorToken() != stored {
		t.Fatalf("live token = %q, want %q", currentOperatorToken(), stored)
	}
	if _, err := os.Stat(operatorTokenFilePath()); !os.IsNotExist(err) {
		t.Fatalf("explicit keychain resolve wrote file backend (stat err = %v)", err)
	}
}

func TestLoadOrCreateOperatorToken_KeychainHit(t *testing.T) {
	withTokenTestEnv(t)
	const stored = "tclo_already_stored_value"
	keychainGet = func(svc, user string) (string, error) {
		if svc != keychainService || user != keychainUser {
			t.Fatalf("unexpected keychain lookup %q/%q", svc, user)
		}
		return stored, nil
	}
	setCalled := false
	keychainSet = func(string, string, string) error { setCalled = true; return nil }

	tok, src := loadOrCreateOperatorTokenKeychain()
	if src.kind != tokenSourceKeychain {
		t.Fatalf("source = %q, want keychain", src.kind)
	}
	if tok != stored {
		t.Fatalf("token = %q, want %q", tok, stored)
	}
	if setCalled {
		t.Fatal("keychainSet should not be called on a hit")
	}
	// A keychain hit must not write the independent file backend.
	if _, err := os.Stat(operatorTokenFilePath()); !os.IsNotExist(err) {
		t.Fatalf("keychain hit wrote a file (stat err = %v)", err)
	}
}

func TestLoadOrCreateOperatorToken_KeychainEmptyMintsAndStores(t *testing.T) {
	withTokenTestEnv(t)
	keychainGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	var setTok string
	keychainSet = func(_, _, secret string) error { setTok = secret; return nil }

	tok, src := loadOrCreateOperatorTokenKeychain()
	if src.kind != tokenSourceKeychain {
		t.Fatalf("source = %q, want keychain", src.kind)
	}
	if !strings.HasPrefix(tok, humanTokenPrefix) {
		t.Fatalf("token %q missing prefix", tok)
	}
	if setTok != tok {
		t.Fatalf("stored %q != returned %q", setTok, tok)
	}
	if _, err := os.Stat(operatorTokenFilePath()); !os.IsNotExist(err) {
		t.Fatal("keychain-store path should not write the file backend")
	}
}

func TestLoadOrCreateOperatorToken_KeychainStoreFailureDoesNotWriteFile(t *testing.T) {
	fp := withTokenTestEnv(t)
	keychainGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	keychainSet = func(string, string, string) error { return errors.New("set denied") }

	tok, src := loadOrCreateOperatorTokenKeychain()
	if src.kind != tokenSourceEphemeral {
		t.Fatalf("source = %q, want ephemeral", src.kind)
	}
	if !strings.HasPrefix(tok, humanTokenPrefix) {
		t.Fatalf("token %q missing prefix", tok)
	}
	if _, err := os.Stat(fp); !os.IsNotExist(err) {
		t.Fatalf("explicit keychain failure touched file backend (stat err = %v)", err)
	}
}

func TestLoadOrCreateOperatorToken_KeychainUnavailableDoesNotReadFile(t *testing.T) {
	fp := withTokenTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
		t.Fatal(err)
	}
	const fileTok = "tclo_existing_file_token"
	if err := os.WriteFile(fp, []byte(fileTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tok, src := loadOrCreateOperatorTokenKeychain()
	if src.kind != tokenSourceEphemeral {
		t.Fatalf("source = %q, want ephemeral", src.kind)
	}
	if tok == fileTok {
		t.Fatal("explicit keychain failure unexpectedly adopted the file token")
	}
	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != fileTok {
		t.Fatalf("file token changed: got %q want %q", strings.TrimSpace(string(got)), fileTok)
	}
}

func TestLoadOrCreateOperatorTokenFile_ReusesExisting(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, ".tclaude", "operator_token")

	tok1, src1, err := loadOrCreateOperatorTokenFile(fp)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if src1.kind != tokenSourceFile {
		t.Fatalf("source = %q, want file", src1.kind)
	}

	tok2, _, err := loadOrCreateOperatorTokenFile(fp)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if tok1 != tok2 {
		t.Fatalf("token not reused: %q -> %q", tok1, tok2)
	}
}

func TestLoadOrCreateOperatorTokenFile_PinnedValueHonored(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, ".tclaude", "operator_token")
	if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
		t.Fatal(err)
	}
	// A human can pin their own token by writing the file directly; we read
	// it verbatim (trimmed), never overwrite it.
	const pinned = "tclo_human_pinned_token"
	if err := os.WriteFile(fp, []byte("  "+pinned+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, src, err := loadOrCreateOperatorTokenFile(fp)
	if err != nil {
		t.Fatalf("read pinned: %v", err)
	}
	if tok != pinned {
		t.Fatalf("token = %q, want %q", tok, pinned)
	}
	if src.kind != tokenSourceFile {
		t.Fatalf("source = %q, want file", src.kind)
	}
}

func TestLoadOrCreateOperatorTokenFile_EmptyPathErrors(t *testing.T) {
	if _, _, err := loadOrCreateOperatorTokenFile(""); err == nil {
		t.Fatal("expected error for empty path")
	}
}

func TestLoadOrCreateOperatorToken_KeychainDoesNotMigrateExistingFileToken(t *testing.T) {
	fp := withTokenTestEnv(t)
	if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
		t.Fatal(err)
	}
	const fileTok = "tclo_from_earlier_fileonly_boot"
	if err := os.WriteFile(fp, []byte(fileTok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	keychainGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	var stored string
	keychainSet = func(_, _, secret string) error { stored = secret; return nil }

	tok, src := loadOrCreateOperatorTokenKeychain()
	if src.kind != tokenSourceKeychain {
		t.Fatalf("source = %q, want keychain", src.kind)
	}
	if tok == fileTok {
		t.Fatalf("keychain unexpectedly adopted file token %q", fileTok)
	}
	if stored != tok {
		t.Fatalf("keychain stored %q, want newly minted %q", stored, tok)
	}
	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != fileTok {
		t.Fatalf("file token changed during keychain opt-in: got %q want %q", strings.TrimSpace(string(got)), fileTok)
	}
}

// TestWriteOperatorTokenBanner_SecretOnlyOnTTY is the security-relevant test:
// the token bytes must appear ONLY on the TTY path, never on any non-TTY
// branch (which could be a log file).
func TestWriteOperatorTokenBanner_SecretOnlyOnTTY(t *testing.T) {
	const secret = "tclo_super_secret_value"
	srcs := []tokenSource{
		{kind: tokenSourceEphemeral},
		{kind: tokenSourceKeychain},
		{kind: tokenSourceFile, path: "/home/u/.tclaude/operator_token"},
	}
	for _, src := range srcs {
		t.Run(string(src.kind)+"/tty", func(t *testing.T) {
			var buf bytes.Buffer
			writeOperatorTokenBanner(&buf, secret, src, true)
			if !strings.Contains(buf.String(), secret) {
				t.Fatalf("TTY banner for %q omitted the export line/secret:\n%s", src.kind, buf.String())
			}
		})
		t.Run(string(src.kind)+"/non-tty", func(t *testing.T) {
			var buf bytes.Buffer
			writeOperatorTokenBanner(&buf, secret, src, false)
			if strings.Contains(buf.String(), secret) {
				t.Fatalf("non-TTY banner for %q LEAKED the secret:\n%s", src.kind, buf.String())
			}
		})
	}
}

func TestMaybeWriteOperatorTokenBanner_NoPrintSuppresses(t *testing.T) {
	const secret = "tclo_super_secret_value"
	src := tokenSource{kind: tokenSourceEphemeral}

	// noPrint=true: nothing written, even on a TTY where the secret would
	// otherwise appear.
	var suppressed bytes.Buffer
	maybeWriteOperatorTokenBanner(&suppressed, secret, src, true, true)
	if suppressed.Len() != 0 {
		t.Fatalf("--no-print-human-token should suppress the banner entirely, got:\n%s", suppressed.String())
	}

	// noPrint=false: behaves exactly like writeOperatorTokenBanner.
	var printed, want bytes.Buffer
	maybeWriteOperatorTokenBanner(&printed, secret, src, true, false)
	writeOperatorTokenBanner(&want, secret, src, true)
	if printed.String() != want.String() {
		t.Fatalf("noPrint=false diverged from writeOperatorTokenBanner:\n got: %q\nwant: %q", printed.String(), want.String())
	}
}

func TestLoadOrCreateOperatorTokenFile_ReChmodsLoosePerms(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, ".tclaude", "operator_token")
	if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fp, []byte("tclo_loose\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateOperatorTokenFile(fp); err != nil {
		t.Fatalf("read: %v", err)
	}
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm after read = %o, want 0600 (defensive re-chmod)", perm)
	}
}

// TestLoadOrCreateOperatorTokenFile_ForcesPermsWhenOverwritingEmptyFile
// guards the secret-at-loose-perms hole: os.WriteFile does NOT re-mode an
// existing file, so writing the freshly-minted secret into a pre-existing
// empty 0644 file must still end at 0600.
func TestLoadOrCreateOperatorTokenFile_ForcesPermsWhenOverwritingEmptyFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, ".tclaude", "operator_token")
	if err := os.MkdirAll(filepath.Dir(fp), 0o700); err != nil {
		t.Fatal(err)
	}
	// Pre-existing EMPTY file at loose perms — forces the mint-and-write
	// branch (not the read+chmod branch).
	if err := os.WriteFile(fp, []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tok, _, err := loadOrCreateOperatorTokenFile(fp)
	if err != nil {
		t.Fatalf("mint-and-write: %v", err)
	}
	if !strings.HasPrefix(tok, humanTokenPrefix) {
		t.Fatalf("token %q missing prefix (should have minted fresh)", tok)
	}
	fi, err := os.Stat(fp)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm after overwriting empty 0644 file = %o, want 0600", perm)
	}
	got, err := os.ReadFile(fp)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != tok {
		t.Fatalf("file contents %q != minted token %q", strings.TrimSpace(string(got)), tok)
	}
}

func TestLoadOrCreateOperatorToken_DegradesToEphemeralWhenFileUnavailable(t *testing.T) {
	withTokenTestEnv(t)
	operatorTokenFilePath = func() string { return "" }
	tok, src := loadOrCreateOperatorTokenFileBacked()
	if src.kind != tokenSourceEphemeral {
		t.Fatalf("source = %q, want ephemeral", src.kind)
	}
	if !strings.HasPrefix(tok, humanTokenPrefix) {
		t.Fatalf("token %q missing prefix", tok)
	}
}
