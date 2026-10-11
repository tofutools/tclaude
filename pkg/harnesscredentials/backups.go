package harnesscredentials

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// BackupInfo contains metadata only. Credential contents have no read API.
type BackupInfo struct {
	ID        string    `json:"id"`
	Harness   string    `json:"harness"`
	CreatedAt time.Time `json:"created_at"`
	Location  string    `json:"location"`
}
type RestoreReceipt struct {
	Receipt
	RestoredFrom string `json:"restored_from"`
	Restored     bool   `json:"restored"`
}

func ValidBackupID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

func saveBackup(privateDir string, previous backup) (Receipt, error) {
	previous.CreatedAt = time.Now().UTC()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Receipt{}, err
	}
	id := hex.EncodeToString(nonce[:])
	root, err := openDirectory(privateDir, true)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = root.Close() }()
	raw, err := json.Marshal(previous)
	if err != nil {
		return Receipt{}, err
	}
	if err := atomicFile(root, id+".json", raw); err != nil {
		return Receipt{}, fmt.Errorf("private credential backup failed")
	}
	return Receipt{BackupID: id, BackupLocation: filepath.Join(privateDir, id+".json")}, nil
}
func readBackup(root *os.Root, id string) (backup, error) {
	var b backup
	if !ValidBackupID(id) {
		return b, fmt.Errorf("invalid backup ID")
	}
	info, err := root.Lstat(id + ".json")
	if err != nil {
		return b, fmt.Errorf("credential backup unavailable")
	}
	if info.Mode().Perm()&0077 != 0 {
		return b, fmt.Errorf("credential backup must be owner-only")
	}
	raw, err := readBoundedFile(root, id+".json", 64<<10)
	if err != nil {
		return b, err
	}
	if json.Unmarshal(raw, &b) != nil {
		return backup{}, fmt.Errorf("invalid credential backup")
	}
	_, names, err := location("/unused", b.Harness, true)
	if err != nil {
		return backup{}, err
	}
	seen := map[string]bool{}
	total := 0
	for _, f := range b.Files {
		allowed := false
		for _, n := range names {
			allowed = allowed || n == f.Name
		}
		if !allowed || seen[f.Name] || (!f.Exists && len(f.Data) != 0) {
			return backup{}, fmt.Errorf("invalid credential backup")
		}
		seen[f.Name] = true
		total += len(f.Data)
	}
	if len(b.Files) == 0 || total > len(names)*MaxBytes {
		return backup{}, fmt.Errorf("invalid credential backup")
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = info.ModTime().UTC()
	}
	return b, nil
}
func listBackups(privateDir, harness string) ([]BackupInfo, error) {
	if _, _, err := location("/unused", harness, true); err != nil {
		return nil, err
	}
	entries := []BackupInfo{}
	if _, err := os.Lstat(privateDir); os.IsNotExist(err) {
		return entries, nil
	}
	root, err := openDirectory(privateDir, false)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	files, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		id := strings.TrimSuffix(f.Name(), ".json")
		if f.Name() != id+".json" || !ValidBackupID(id) {
			continue
		}
		b, err := readBackup(root, id)
		if err != nil {
			continue
		}
		if b.Harness == harness {
			entries = append(entries, BackupInfo{ID: id, Harness: harness, CreatedAt: b.CreatedAt, Location: filepath.Join(privateDir, f.Name())})
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].ID > entries[j].ID
		}
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})
	return entries, nil
}
func ListBackups(privateDir, harness string) ([]BackupInfo, error) {
	mu.Lock()
	defer mu.Unlock()
	return listBackups(privateDir, harness)
}
func previousFiles(root *os.Root, names []string) ([]savedFile, error) {
	files := []savedFile{}
	for _, n := range names {
		raw, err := readFile(root, n)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		files = append(files, savedFile{Name: n, Exists: err == nil, Data: raw})
	}
	return files, nil
}
func Backup(home, privateDir, harness string, authorize func() bool) (Receipt, error) {
	mu.Lock()
	defer mu.Unlock()
	if !authorize() {
		return Receipt{}, fmt.Errorf("credential receiving authority revoked")
	}
	dir, names, err := location(home, harness, true)
	if err != nil {
		return Receipt{}, err
	}
	root, err := openDirectory(dir, true)
	if err != nil {
		return Receipt{}, err
	}
	defer func() { _ = root.Close() }()
	previous, err := previousFiles(root, names)
	if err != nil {
		return Receipt{}, err
	}
	return saveBackup(privateDir, backup{Harness: harness, Files: previous})
}

// Restore saves the current files first, so the restore itself is reversible.
// An empty ID selects the newest matching backup before this safety backup.
func Restore(home, privateDir, harness, id string, authorize func() bool) (RestoreReceipt, error) {
	mu.Lock()
	defer mu.Unlock()
	out := RestoreReceipt{}
	if !authorize() {
		return out, fmt.Errorf("credential receiving authority revoked")
	}
	dir, _, err := location(home, harness, true)
	if err != nil {
		return out, err
	}
	if id == "" {
		entries, err := listBackups(privateDir, harness)
		if err != nil {
			return out, err
		}
		if len(entries) == 0 {
			return out, fmt.Errorf("no credential backup found")
		}
		id = entries[0].ID
	}
	backups, err := openDirectory(privateDir, false)
	if err != nil {
		return out, err
	}
	defer func() { _ = backups.Close() }()
	b, err := readBackup(backups, id)
	if err != nil {
		return out, err
	}
	if b.Harness != harness {
		return out, fmt.Errorf("backup belongs to another harness")
	}
	root, err := openDirectory(dir, true)
	if err != nil {
		return out, err
	}
	defer func() { _ = root.Close() }()
	names := []string{}
	for _, f := range b.Files {
		names = append(names, f.Name)
	}
	previous, err := previousFiles(root, names)
	if err != nil {
		return out, err
	}
	out.Receipt, err = saveBackup(privateDir, backup{Harness: harness, Files: previous})
	if err != nil {
		return out, err
	}
	out.RestoredFrom = id
	for i, f := range b.Files {
		if !authorize() {
			return out, errors.Join(fmt.Errorf("credential receiving authority revoked"), restoreFiles(root, previous[:i]))
		}
		raw, e := readFile(root, f.Name)
		p := previous[i]
		if p.Exists && (e != nil || string(raw) != string(p.Data)) || !p.Exists && !os.IsNotExist(e) {
			return out, errors.Join(fmt.Errorf("credentials changed before restoration"), restoreFiles(root, previous[:i]))
		}
		if err := restoreFiles(root, []savedFile{f}); err != nil {
			return out, errors.Join(err, restoreFiles(root, previous[:i+1]))
		}
	}
	out.Restored = true
	return out, nil
}
