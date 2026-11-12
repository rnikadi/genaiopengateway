package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rnikadi/genaiopengateway/internal/config"
	"github.com/rnikadi/genaiopengateway/internal/store"
)

// subcommand runs `backup` or `restore` when args name one, and reports
// whether it did (docs/backup.md).
func subcommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	var err error
	switch args[0] {
	case "backup":
		err = runBackup(args[1:], stdout, stderr)
	case "restore":
		err = runRestore(args[1:], stdout)
	default:
		return false, 0
	}
	if errors.Is(err, flag.ErrHelp) {
		return true, 2
	}
	if err != nil {
		fmt.Fprintln(stderr, "error:", err)
		return true, 1
	}
	return true, 0
}

func storeFlags(fs *flag.FlagSet) (path, dataDir *string) {
	path = fs.String("config", "config.yaml", "the gateway's config file; without one, the zero-config data directory is used")
	dataDir = fs.String("data", envOr("GOG_DATA_DIR", config.DefaultDataDir), "data directory of a gateway started without a config file")
	return
}

// openStore opens the store a gateway with this config uses, and returns
// the key check of its encryption key and of its previous keys.
func openStore(ctx context.Context, path, dataDir string) (*store.Store, []string, error) {
	cfg, _, err := loadConfig(path, dataDir)
	if err != nil {
		return nil, nil, err
	}
	if cfg.Store.Driver == "" {
		return nil, nil, errors.New("this config has no store, so there is nothing to back up or restore")
	}
	st, err := store.Open(ctx, cfg.Store.Driver, cfg.Store.DSN.Value())
	if err != nil {
		return nil, nil, fmt.Errorf("open store: %w", err)
	}
	var checks []string
	for _, k := range append([]config.Secret{cfg.Secrets.EncryptionKey}, cfg.Secrets.PreviousKeys...) {
		if c := store.KeyCheck(k.Value()); c != "" {
			checks = append(checks, c)
		}
	}
	return st, checks, nil
}

func runBackup(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: genaiopengateway backup [-config FILE | -data DIR] -out FILE\n\nWrites a consistent snapshot of the gateway's database while it keeps serving.")
		fs.PrintDefaults()
	}
	path, dataDir := storeFlags(fs)
	out := fs.String("out", "", "where to write the archive; - writes to standard output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		fs.Usage()
		return errors.New("-out is required")
	}
	ctx := context.Background()
	st, checks, err := openStore(ctx, *path, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	opts := store.BackupOptions{}
	if len(checks) > 0 {
		opts.KeyCheck = checks[0]
	}
	if *out == "-" {
		_, err := st.Backup(ctx, stdout, opts)
		return err
	}
	// Write next to the target and rename, so a failed backup never
	// leaves a partial file under the expected name.
	tmp, err := os.CreateTemp(filepath.Dir(*out), ".gog-backup-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h, err := st.Backup(ctx, tmp, opts)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), *out); err != nil {
		return err
	}
	fmt.Fprintf(stderr, "Backed up %s to %s (schema %s).\n", summary(h.Tables), *out, h.Schema)
	return nil
}

func runRestore(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: genaiopengateway restore [-config FILE | -data DIR] [-replace] [-check] ARCHIVE\n\nLoads a backup into the gateway's database, all or nothing. The database\nmay use a different driver from the one the backup came from.")
		fs.PrintDefaults()
	}
	path, dataDir := storeFlags(fs)
	replace := fs.Bool("replace", false, "delete what the database holds first; without it, only an empty database is restored into")
	check := fs.Bool("check", false, "only read the archive and say what it holds")
	ignoreKey := fs.Bool("ignore-key", false, "restore even though this gateway's encryption key is not the one the archive's secrets need")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("name one archive to restore")
	}
	file := fs.Arg(0)
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	h, err := store.ReadBackupHeader(f)
	f.Close()
	if err != nil {
		return err
	}
	if *check {
		fmt.Fprintf(stdout, "Backup taken %s from %s, schema %s: %s.\n", h.CreatedAt.Format("2006-01-02 15:04 MST"), h.Driver, h.Schema, summary(h.Tables))
		return nil
	}
	ctx := context.Background()
	st, checks, err := openStore(ctx, *path, *dataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	if h.KeyCheck != "" && !*ignoreKey && !contains(checks, h.KeyCheck) {
		return errors.New("this gateway's secrets.encryption_key (or previous_keys) is not the key the backup's stored secrets were encrypted with, so provider keys and other secrets could not be read after the restore; configure that key, or pass -ignore-key to restore anyway")
	}
	f, err = os.Open(file)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := st.Restore(ctx, f, store.RestoreOptions{Replace: *replace}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Restored %s from %s. Running gateways pick up the configuration within store.refresh_interval; people sign in again.\n", summary(h.Tables), file)
	return nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// summary describes the main tables' row counts in words.
func summary(tables map[string]int64) string {
	names := map[string]string{"orgs": "organizations", "teams": "teams", "api_keys": "keys", "config_objects": "configuration objects", "usage": "usage records", "audit_log": "audit entries"}
	var keys []string
	for k := range names {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return order(keys[i]) < order(keys[j]) })
	var parts []string
	var rest int64
	for t, n := range tables {
		if _, ok := names[t]; !ok {
			rest += n
		}
	}
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%d %s", tables[k], names[k]))
	}
	return strings.Join(parts, ", ") + fmt.Sprintf(" and %d other rows", rest)
}

func order(t string) int {
	return strings.Index("orgs teams api_keys config_objects usage audit_log", t)
}
