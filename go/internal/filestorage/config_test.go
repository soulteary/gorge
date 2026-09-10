package filestorage

import (
	"os"
	"testing"
)

// clearEnv unsets everything LoadFromEnv reads, so a default is really a
// default and not whatever the developer's shell happens to export.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"GORGE_LISTEN_ADDR", "LISTEN_ADDR", "GORGE_SERVICE_TOKEN", "SERVICE_TOKEN",
		"GORGE_FILE_MYSQL_HOST", "MYSQL_HOST",
		"GORGE_FILE_MYSQL_PORT", "MYSQL_PORT",
		"GORGE_FILE_MYSQL_USER", "MYSQL_USER",
		"GORGE_FILE_MYSQL_PASS", "MYSQL_PASS",
		"GORGE_FILE_NAMESPACE", "STORAGE_NAMESPACE",
		"GORGE_FILE_MYSQL_BLOB_MAX_SIZE", "MYSQL_BLOB_MAX_SIZE",
		"GORGE_FILE_LOCAL_DISK_PATH", "LOCAL_DISK_PATH",
		"GORGE_FILE_S3_BUCKET", "S3_BUCKET",
		"GORGE_FILE_S3_ACCESS_KEY", "S3_ACCESS_KEY",
		"GORGE_FILE_S3_SECRET_KEY", "S3_SECRET_KEY",
		"GORGE_FILE_S3_REGION", "S3_REGION",
		"GORGE_FILE_S3_ENDPOINT", "S3_ENDPOINT",
		"GORGE_FILE_INSTANCE_NAME", "INSTANCE_NAME",
	} {
		if _, set := os.LookupEnv(key); set {
			t.Setenv(key, "")
			_ = os.Unsetenv(key)
		}
	}
}

// TestNoBackendIsConfiguredByDefault is the deliberate change from the
// standalone service, and the one that matters most: there the MySQL host
// defaulted to 127.0.0.1 and the blob limit to 1 MB, so an unconfigured
// service registered a blob backend against a database that does not exist —
// which took priority 1, collected every small upload, failed it, and made
// /readyz report the whole service unavailable. Now "nothing configured" is a
// state the service can recognise.
func TestNoBackendIsConfiguredByDefault(t *testing.T) {
	clearEnv(t)
	cfg := LoadFromEnv()

	if cfg.MySQLBlobEnabled() {
		t.Error("the blob backend must stay off until a MySQL host is given")
	}
	if cfg.LocalDiskEnabled() {
		t.Error("local disk must stay off until a path is given")
	}
	if cfg.S3Enabled() {
		t.Error("S3 must stay off until it is fully configured")
	}
	if cfg.ListenAddr != DefaultListenAddr {
		t.Errorf("ListenAddr = %q, want %q", cfg.ListenAddr, DefaultListenAddr)
	}
	if cfg.MySQLBlobMaxSize != DefaultMySQLBlobMaxSize {
		t.Errorf("MySQLBlobMaxSize = %d, want %d", cfg.MySQLBlobMaxSize, DefaultMySQLBlobMaxSize)
	}
}

func TestBlobBackendSwitches(t *testing.T) {
	t.Run("a host turns it on", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("GORGE_FILE_MYSQL_HOST", "db")
		if !LoadFromEnv().MySQLBlobEnabled() {
			t.Error("a configured host should enable the blob backend")
		}
	})

	t.Run("a zero limit turns it off again", func(t *testing.T) {
		// This is how Phorge's own docs tell an operator to disable the MySQL
		// engine, so the same value has to work here.
		clearEnv(t)
		t.Setenv("GORGE_FILE_MYSQL_HOST", "db")
		t.Setenv("GORGE_FILE_MYSQL_BLOB_MAX_SIZE", "0")
		if LoadFromEnv().MySQLBlobEnabled() {
			t.Error("a zero size limit should disable the blob backend")
		}
	})
}

func TestS3RequiresEverySetting(t *testing.T) {
	clearEnv(t)
	// A partial configuration builds a client that fails every request, which
	// is a worse outcome than not registering the backend at all.
	t.Setenv("GORGE_FILE_S3_BUCKET", "files")
	t.Setenv("GORGE_FILE_S3_ACCESS_KEY", "AKID")
	if LoadFromEnv().S3Enabled() {
		t.Fatal("a partial S3 configuration must not enable the backend")
	}

	t.Setenv("GORGE_FILE_S3_SECRET_KEY", "secret")
	t.Setenv("GORGE_FILE_S3_REGION", "us-east-1")
	t.Setenv("GORGE_FILE_S3_ENDPOINT", "https://s3.example.com")
	if !LoadFromEnv().S3Enabled() {
		t.Error("a complete S3 configuration should enable the backend")
	}
}

// TestRetiredNamesAreIgnored prevents the removed standalone aliases from
// silently reappearing in this security-sensitive storage configuration.
func TestRetiredNamesAreIgnored(t *testing.T) {
	clearEnv(t)
	t.Setenv("LOCAL_DISK_PATH", "/legacy")
	t.Setenv("MYSQL_HOST", "legacy-db")
	t.Setenv("MYSQL_BLOB_MAX_SIZE", "111")

	if cfg := LoadFromEnv(); cfg.LocalDiskPath != "" || cfg.MySQLHost != "" ||
		cfg.MySQLBlobMaxSize != DefaultMySQLBlobMaxSize {
		t.Fatalf("retired names must be ignored: %+v", cfg)
	}

	t.Setenv("GORGE_FILE_LOCAL_DISK_PATH", "/current")
	t.Setenv("GORGE_FILE_MYSQL_HOST", "current-db")
	t.Setenv("GORGE_FILE_MYSQL_BLOB_MAX_SIZE", "222")

	cfg := LoadFromEnv()
	if cfg.LocalDiskPath != "/current" || cfg.MySQLHost != "current-db" || cfg.MySQLBlobMaxSize != 222 {
		t.Errorf("the prefixed names must win: %+v", cfg)
	}
}

// TestFileDSN pins the database name, which is not this service's choice: the
// blob backend writes Phorge's own `{namespace}_file` database, and a DSN
// pointing anywhere else finds no table.
func TestFileDSN(t *testing.T) {
	cfg := &Config{
		MySQLUser: "phorge",
		MySQLPass: "secret",
		MySQLHost: "db",
		MySQLPort: 3306,
		Namespace: "phorge",
	}

	const want = "phorge:secret@tcp(db:3306)/phorge_file?parseTime=true&timeout=5s&readTimeout=30s&writeTimeout=30s"
	if got := cfg.FileDSN(); got != want {
		t.Errorf("FileDSN() =\n%q\nwant\n%q", got, want)
	}
}

func TestNewRouterFromConfig(t *testing.T) {
	t.Run("no backend configured is not an error", func(t *testing.T) {
		quietLogs(t)
		clearEnv(t)

		router, err := NewRouterFromConfig(LoadFromEnv())
		if err != nil {
			t.Fatalf("an unconfigured service must still start: %v", err)
		}
		defer func() { _ = router.Close() }()

		if err := router.Ready(); err == nil {
			t.Error("...but it must not report itself ready")
		}
	})

	t.Run("a local disk path registers the backend", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("GORGE_FILE_LOCAL_DISK_PATH", t.TempDir())

		router, err := NewRouterFromConfig(LoadFromEnv())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = router.Close() }()

		info := router.ListEngines()
		if len(info) != 1 || info[0].Identifier != identifierLocalDisk {
			t.Fatalf("expected only the local disk engine, got %+v", info)
		}
		if err := router.Ready(); err != nil {
			t.Errorf("a local disk backend has nothing to probe: %v", err)
		}
	})

	t.Run("a configured backend that cannot be built fails the start", func(t *testing.T) {
		clearEnv(t)
		// A relative path is rejected by the engine. Starting anyway would
		// leave a service that looks configured and stores nothing.
		t.Setenv("GORGE_FILE_LOCAL_DISK_PATH", "relative/path")

		if _, err := NewRouterFromConfig(LoadFromEnv()); err == nil {
			t.Error("expected the start to fail")
		}
	})
}
