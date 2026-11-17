// BloodHound CE self-extracting launcher.
//
// One executable that carries the BloodHound API/UI binary and a complete
// PostgreSQL server. On first start it extracts both under a per-user data
// directory, initializes a private cluster, starts PostgreSQL on loopback,
// writes a bhapi configuration that uses PostgreSQL as the graph driver, and
// then runs the BloodHound server.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

//go:embed payload/payload.zip
var payload []byte

const (
	appName    = "BloodHound CE"
	dbUser     = "bloodhound"
	dbName     = "bloodhound"
	stateFile  = "launcher.json"
	secretFile = "secrets.json"
	lockFile   = "launcher.lock"
)

var (
	noBrowser   bool
	homeDirFlag string
	execLog     *os.File
)

// attachExecLog hands child processes a real file handle. Passing an io.Writer
// (for example log.Writer()) makes os/exec create a pipe, and on Windows
// postgres.exe inherits the write end from pg_ctl: pg_ctl exits, but Wait blocks
// forever waiting for EOF on the pipe.
func attachExecLog(cmd *exec.Cmd) {
	if execLog != nil {
		cmd.Stdout = execLog
		cmd.Stderr = execLog
	} else {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
	}
}

type secrets struct {
	DBPassword    string `json:"db_password"`
	AdminPassword string `json:"admin_password"`
	JWTSigningKey string `json:"jwt_signing_key"`
}

type state struct {
	HTTPPort int `json:"http_port"`
	PGPort   int `json:"pg_port"`
}

func main() {
	flag.BoolVar(&noBrowser, "no-browser", false, "do not open the web UI in a browser")
	flag.StringVar(&homeDirFlag, "home", "", "data directory (default: per-user application data)")
	flag.Parse()

	log.SetFlags(log.LstdFlags)
	log.SetPrefix("[bh] ")

	if err := run(); err != nil {
		if errors.Is(err, errAlreadyRunning) {
			return
		}
		log.Printf("FATAL: %v", err)
		os.Exit(1)
	}
}

var errAlreadyRunning = errors.New("already running")

func run() error {
	defer signalConsoleDone()

	base, err := dataHome()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	logDir := filepath.Join(base, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return err
	}
	if logFile, err := os.OpenFile(filepath.Join(logDir, "launcher.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		execLog = logFile
		log.SetOutput(io.MultiWriter(os.Stdout, logFile))
		defer logFile.Close()
	}

	// If a previous launcher left a server running, just surface it.
	if st, err := readState(base); err == nil && st.HTTPPort > 0 && healthy(st.HTTPPort) {
		url := uiURL(st.HTTPPort)
		log.Printf("%s is already running at %s", appName, url)
		openUI(url)
		return errAlreadyRunning
	}

	unlock, err := acquireLock(base)
	if err != nil {
		return err
	}
	defer unlock()

	appDir, err := ensurePayload(base)
	if err != nil {
		return fmt.Errorf("extract payload: %w", err)
	}

	sec, created, err := loadOrCreateSecrets(base)
	if err != nil {
		return err
	}
	if created {
		log.Printf("Generated credentials in %s", filepath.Join(base, secretFile))
	}

	pgdata := filepath.Join(base, "pgdata")
	initdb, pgctl := pgTools(appDir)

	if err := initCluster(initdb, pgdata, sec.DBPassword); err != nil {
		return err
	}

	pgPort, err := freePort()
	if err != nil {
		return err
	}
	pgLog := filepath.Join(logDir, "postgres.log")
	if err := startPostgres(pgctl, pgdata, pgLog, pgPort); err != nil {
		return err
	}
	defer stopPostgres(pgctl, pgdata)

	log.Printf("Preparing database")
	if err := ensureDatabase(pgPort, sec.DBPassword); err != nil {
		return fmt.Errorf("prepare database: %w", err)
	}
	log.Printf("Database ready")

	httpPort, err := pickHTTPPort(base)
	if err != nil {
		return err
	}
	metricsPort, err := freePort()
	if err != nil {
		return err
	}

	workDir := filepath.Join(base, "work")
	collectorsDir := filepath.Join(appDir, "collectors")
	extensionsDir := filepath.Join(appDir, "extensions")
	for _, dir := range []string{workDir, collectorsDir, extensionsDir, filepath.Join(workDir, "tmp"), filepath.Join(workDir, "retained"), filepath.Join(workDir, "ingest_scratch")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	cfgPath := filepath.Join(base, "bhapi.json")
	if err := writeConfig(cfgPath, buildConfig(sec, httpPort, metricsPort, pgPort, workDir, collectorsDir, extensionsDir)); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := writeState(base, state{HTTPPort: httpPort, PGPort: pgPort}); err != nil {
		return err
	}

	log.Printf("Starting %s (PostgreSQL %d, HTTP %d)", appName, pgPort, httpPort)
	log.Printf("Logs: %s", logDir)
	if created {
		log.Printf("Initial admin login: %s / %s", "admin", sec.AdminPassword)
	}

	bhLog, err := os.OpenFile(filepath.Join(logDir, "bloodhound.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer bhLog.Close()

	child, err := startBloodHound(appDir, cfgPath, bhLog)
	if err != nil {
		return err
	}

	url := uiURL(httpPort)
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()

	if err := waitHealthy(httpPort, 5*time.Minute, done); err != nil {
		_ = terminate(child.Process)
		return err
	}
	log.Printf("BloodHound is ready: %s", url)
	log.Printf("Admin login: admin / %s", sec.AdminPassword)
	openUI(url)

	shutdown := func(reason string) {
		log.Printf("%s, shutting down", reason)
		_ = terminate(child.Process)
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			log.Printf("BloodHound did not exit; killing it")
			_ = child.Process.Kill()
			<-done
		}
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case sig := <-sigCh:
		shutdown(fmt.Sprintf("Received %v", sig))
	case <-consoleClose:
		shutdown("Console closing")
	case err := <-done:
		if err != nil {
			log.Printf("BloodHound exited: %v", err)
		} else {
			log.Printf("BloodHound exited")
		}
	}

	return nil
}

func dataHome() (string, error) {
	if homeDirFlag != "" {
		return filepath.Abs(homeDirFlag)
	}
	if override := os.Getenv("BLOODHOUND_CE_HOME"); override != "" {
		return filepath.Abs(override)
	}
	if runtime.GOOS == "windows" {
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "BloodHoundCE"), nil
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "BloodHoundCE"), nil
	}
	return "", errors.New("cannot determine a per-user data directory")
}

func acquireLock(base string) (func(), error) {
	path := filepath.Join(base, lockFile)
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create lock file: %w", err)
		}

		// A second copy may have been started while the first was still
		// initializing. Give it a moment, then take over the stale lock.
		log.Printf("Instance lock exists; waiting for another instance")
		for range 15 {
			time.Sleep(time.Second)
			if st, err := readState(base); err == nil && st.HTTPPort > 0 && healthy(st.HTTPPort) {
				url := uiURL(st.HTTPPort)
				log.Printf("%s is already running at %s", appName, url)
				openUI(url)
				return nil, errAlreadyRunning
			}
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("remove stale lock: %w", err)
		}
	}
	return nil, errors.New("could not acquire the instance lock")
}

func ensurePayload(base string) (string, error) {
	appDir := filepath.Join(base, "app")
	wantID, err := payloadID()
	if err != nil {
		return "", err
	}
	if got, err := os.ReadFile(filepath.Join(appDir, "payload.id")); err == nil && strings.TrimSpace(string(got)) == wantID {
		return appDir, nil
	}

	log.Printf("Extracting embedded payload (first run or update)")
	tmp := appDir + ".tmp"
	if err := os.RemoveAll(tmp); err != nil {
		return "", err
	}
	if err := extractZip(tmp); err != nil {
		os.RemoveAll(tmp)
		return "", err
	}
	if err := os.RemoveAll(appDir); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, appDir); err != nil {
		return "", err
	}
	return appDir, nil
}

func payloadID() (string, error) {
	zr, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return "", err
	}
	for _, f := range zr.File {
		if f.Name == "payload.id" {
			rc, err := f.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			id, err := io.ReadAll(rc)
			return strings.TrimSpace(string(id)), err
		}
	}
	return "", errors.New("payload.id missing from embedded payload")
}

func extractZip(dest string) error {
	zr, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		name := filepath.FromSlash(f.Name)
		target := filepath.Join(dest, name)
		if rel, err := filepath.Rel(dest, target); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe path in payload: %s", f.Name)
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		if strings.HasPrefix(f.Name, "pgsql/bin/") || f.Name == "bloodhound" {
			mode = 0o755
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			out.Close()
			rc.Close()
			return err
		}
		out.Close()
		rc.Close()
	}
	return nil
}

func loadOrCreateSecrets(base string) (secrets, bool, error) {
	path := filepath.Join(base, secretFile)
	if data, err := os.ReadFile(path); err == nil {
		var sec secrets
		if err := json.Unmarshal(data, &sec); err != nil {
			return sec, false, err
		}
		if sec.DBPassword == "" || sec.AdminPassword == "" || sec.JWTSigningKey == "" {
			return sec, false, errors.New("secrets file is incomplete")
		}
		return sec, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return secrets{}, false, err
	}

	sec := secrets{
		DBPassword:    randomToken(32),
		AdminPassword: randomPassword(24),
		JWTSigningKey: base64.StdEncoding.EncodeToString(randomBytes(32)),
	}
	data, err := json.MarshalIndent(sec, "", "  ")
	if err != nil {
		return sec, false, err
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return sec, false, err
	}
	return sec, true, nil
}

func randomBytes(n int) []byte {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		panic(err)
	}
	return buf
}

func randomToken(n int) string {
	return base64.RawURLEncoding.EncodeToString(randomBytes(n))[:n]
}

func randomPassword(n int) string {
	const alphabet = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := randomBytes(n)
	for i := range buf {
		buf[i] = alphabet[int(buf[i])%len(alphabet)]
	}
	return string(buf)
}

func pgTools(appDir string) (string, string) {
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	bin := filepath.Join(appDir, "pgsql", "bin")
	return filepath.Join(bin, "initdb"+exe), filepath.Join(bin, "pg_ctl"+exe)
}

func initCluster(initdb, pgdata, password string) error {
	if _, err := os.Stat(filepath.Join(pgdata, "PG_VERSION")); err == nil {
		return nil
	}
	log.Printf("Initializing PostgreSQL cluster at %s", pgdata)
	if err := os.MkdirAll(pgdata, 0o700); err != nil {
		return err
	}

	pwfile := filepath.Join(os.TempDir(), "bh-pg-pw-"+randomToken(8))
	if err := os.WriteFile(pwfile, []byte(password), 0o600); err != nil {
		return err
	}
	defer os.Remove(pwfile)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, initdb,
		"-D", pgdata,
		"-U", dbUser,
		"-A", "scram-sha-256",
		"--pwfile="+pwfile,
		"-E", "UTF8",
		"--locale=C",
	)
	attachExecLog(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("initdb: %w", err)
	}
	return nil
}

func startPostgres(pgctl, pgdata, logPath string, port int) error {
	// Reap a postmaster left behind by a crashed launcher.
	if exec.Command(pgctl, "-D", pgdata, "status").Run() == nil {
		log.Printf("Stopping stale PostgreSQL instance")
		_ = exec.Command(pgctl, "-D", pgdata, "-m", "fast", "-w", "-t", "30", "stop").Run()
	}

	log.Printf("Starting PostgreSQL on 127.0.0.1:%d", port)
	opts := fmt.Sprintf("-p %d -h 127.0.0.1 -c shared_buffers=256MB -c max_connections=100", port)
	cmd := exec.Command(pgctl, "-D", pgdata, "-l", logPath, "-o", opts, "-w", "-t", "120", "start")
	attachExecLog(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("start PostgreSQL: %w (see %s)", err, logPath)
	}
	return nil
}

func stopPostgres(pgctl, pgdata string) {
	cmd := exec.Command(pgctl, "-D", pgdata, "-m", "fast", "-w", "-t", "60", "stop")
	attachExecLog(cmd)
	if err := cmd.Run(); err != nil {
		log.Printf("Stopping PostgreSQL: %v", err)
	}
}

func ensureDatabase(port int, password string) error {
	adminDSN := postgresDSN(port, dbUser, password, "postgres")
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn, err := pgx.Connect(ctx, adminDSN)
		if err == nil {
			var exists bool
			err = conn.QueryRow(ctx, "select exists(select 1 from pg_database where datname = $1)", dbName).Scan(&exists)
			if err == nil && !exists {
				_, err = conn.Exec(ctx, "create database "+pgx.Identifier{dbName}.Sanitize())
			}
			conn.Close(ctx)
			cancel()
			if err == nil {
				return nil
			}
		} else {
			cancel()
		}
		lastErr = err
		time.Sleep(time.Second)
	}
	return fmt.Errorf("waiting for PostgreSQL: %w", lastErr)
}

func postgresDSN(port int, user, password, database string) string {
	u := &url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(user, password),
		Host:     net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		Path:     "/" + database,
		RawQuery: "sslmode=disable",
	}
	return u.String()
}

func pickHTTPPort(base string) (int, error) {
	if st, err := readState(base); err == nil && st.HTTPPort > 0 && portFree(st.HTTPPort) {
		return st.HTTPPort, nil
	}
	if portFree(8080) {
		return 8080, nil
	}
	return freePort()
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

func portFree(port int) bool {
	l, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

func buildConfig(sec secrets, httpPort, metricsPort, pgPort int, workDir, collectorsDir, extensionsDir string) map[string]any {
	return map[string]any{
		"version":                       2,
		"bind_addr":                     fmt.Sprintf("127.0.0.1:%d", httpPort),
		"metrics_port":                  fmt.Sprintf("127.0.0.1:%d", metricsPort),
		"root_url":                      fmt.Sprintf("http://127.0.0.1:%d/", httpPort),
		"work_dir":                      workDir,
		"log_level":                     "INFO",
		"graph_driver":                  "pg",
		"database":                      map[string]any{"connection": postgresDSN(pgPort, dbUser, sec.DBPassword, dbName), "max_concurrent_sessions": 10},
		"collectors_base_path":          collectorsDir,
		"embedded_extensions_base_path": extensionsDir,
		"crypto":                        map[string]any{"jwt": map[string]any{"signing_key": sec.JWTSigningKey}},
		"default_admin": map[string]any{
			"enabled":        true,
			"principal_name": "admin",
			"password":       sec.AdminPassword,
			"email_address":  "admin@example.com",
			"first_name":     "Admin",
			"last_name":      "User",
			"expire_now":     false,
		},
		"enable_user_analytics":              false,
		"enable_text_logger":                 true,
		"datapipe_interval":                  60,
		"enable_startup_wait_period":         true,
		"force_download_embedded_collectors": false,
	}
}

func writeConfig(path string, cfg map[string]any) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func writeState(base string, st state) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(base, stateFile), data, 0o644)
}

func readState(base string) (state, error) {
	var st state
	data, err := os.ReadFile(filepath.Join(base, stateFile))
	if err != nil {
		return st, err
	}
	err = json.Unmarshal(data, &st)
	return st, err
}

func startBloodHound(appDir, cfgPath string, output io.Writer) (*exec.Cmd, error) {
	name := "bloodhound"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	cmd := exec.Command(filepath.Join(appDir, name), "-configfile", cfgPath)
	cmd.Dir = appDir
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.Env = cleanEnv()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start BloodHound: %w", err)
	}
	return cmd, nil
}

// cleanEnv drops bhe_* variables so an unrelated environment cannot silently
// override the config written by the launcher.
func cleanEnv() []string {
	out := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		key := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			key = kv[:i]
		}
		if strings.HasPrefix(strings.ToLower(key), "bhe_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func healthy(port int) bool {
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(uiURL(port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}

func waitHealthy(port int, timeout time.Duration, done <-chan error) error {
	deadline := time.Now().Add(timeout)
	next := time.Now()
	for time.Now().Before(deadline) {
		if healthy(port) {
			return nil
		}
		select {
		case err := <-done:
			if err == nil {
				return errors.New("BloodHound exited during startup")
			}
			return fmt.Errorf("BloodHound exited during startup: %w", err)
		default:
		}
		if time.Now().After(next) {
			log.Printf("Waiting for BloodHound to become ready…")
			next = time.Now().Add(10 * time.Second)
		}
		time.Sleep(500 * time.Millisecond)
	}
	return errors.New("BloodHound did not become ready; see logs/bloodhound.log")
}

func uiURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/ui/", port)
}
