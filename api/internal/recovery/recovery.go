// Package recovery performs a retained, isolated control-database restore.
// It never starts a control worker against the restored database and never
// drops databases. This rehearsal does not assert Neon data-plane or HA DR.
package recovery

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Options struct {
	Workspace, PrivateDirectory, EvidenceFile, DatabaseURL, SSHPassword string
}

type Table struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
	SHA256 string `json:"sha256"`
}

type Report struct {
	Result              string     `json:"result"`
	Stage               string     `json:"stage"`
	StartedAt           time.Time  `json:"started_at"`
	FinishedAt          *time.Time `json:"finished_at,omitempty"`
	TargetDatabase      string     `json:"target_database,omitempty"`
	SourcePodUID        string     `json:"source_pod_uid,omitempty"`
	DumpBytes           int64      `json:"dump_bytes,omitempty"`
	DumpSHA256          string     `json:"dump_sha256,omitempty"`
	SourceTables        []Table    `json:"source_tables,omitempty"`
	RestoredTables      []Table    `json:"restored_tables,omitempty"`
	TablesEqual         bool       `json:"tables_equal"`
	ConstraintsValid    bool       `json:"constraints_valid"`
	ArchiveRestored     bool       `json:"archive_restored"`
	CompleteSystemDR    bool       `json:"complete_system_dr"`
	CredentialsRecorded bool       `json:"credential_values_recorded"`
	Limitations         []string   `json:"limitations"`
}

func within(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

// Select only algorithms for host keys already accepted by the exact-host
// known_hosts callback. Servers may advertise an unrecorded Ed25519 key before
// a recorded RSA key; default negotiation must not trigger a trust bypass.
func trustedHostKeyAlgorithms(data []byte, address string, remote net.Addr, verify ssh.HostKeyCallback) ([]string, error) {
	algorithms := []string{}
	seen := map[string]bool{}
	for len(data) > 0 {
		_, _, key, _, rest, err := ssh.ParseKnownHosts(data)
		if err != nil {
			return nil, err
		}
		data = rest
		if verify(address, remote, key) != nil {
			continue
		}
		candidates := []string{key.Type()}
		if key.Type() == ssh.KeyAlgoRSA {
			// The recorded RSA public key also verifies modern RSA SHA2 host
			// signatures. Do not enable deprecated SHA1 ssh-rsa negotiation.
			candidates = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		}
		for _, algorithm := range candidates {
			if !seen[algorithm] {
				algorithms = append(algorithms, algorithm)
				seen[algorithm] = true
			}
		}
	}
	if len(algorithms) == 0 {
		return nil, errors.New("no trusted host key algorithm for exact destination")
	}
	return algorithms, nil
}

func remote(ctx context.Context, client *ssh.Client, command string, input io.Reader, output io.Writer, log io.Writer) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	session.Stdin, session.Stdout, session.Stderr = input, output, log
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()
	select {
	case err = <-done:
		return err
	case <-ctx.Done():
		session.Close()
		<-done
		return ctx.Err()
	}
}

func fingerprint(ctx context.Context, tx pgx.Tx) ([]Table, error) {
	rows, err := tx.Query(ctx, `SELECT n.nspname,c.relname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace
		WHERE c.relkind IN ('r','p') AND n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'
		ORDER BY n.nspname COLLATE "C",c.relname COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	tables := []Table{}
	for rows.Next() {
		var table Table
		if err = rows.Scan(&table.Schema, &table.Name); err != nil {
			rows.Close()
			return nil, err
		}
		tables = append(tables, table)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range tables {
		table := &tables[i]
		query := `SELECT row_to_json(t)::text FROM ` + pgx.Identifier{table.Schema, table.Name}.Sanitize() + ` t ORDER BY row_to_json(t)::text COLLATE "C"`
		data, err := tx.Query(ctx, query)
		if err != nil {
			return nil, err
		}
		hash := sha256.New()
		for data.Next() {
			var value string
			if err = data.Scan(&value); err != nil {
				data.Close()
				return nil, err
			}
			// Length framing makes concatenated row boundaries unambiguous.
			var size [8]byte
			binary.BigEndian.PutUint64(size[:], uint64(len(value)))
			hash.Write(size[:])
			hash.Write([]byte(value))
			table.Rows++
		}
		err = data.Err()
		data.Close()
		if err != nil {
			return nil, err
		}
		table.SHA256 = hex.EncodeToString(hash.Sum(nil))
	}
	return tables, nil
}

// Run uses an exported repeatable-read snapshot for BOTH source fingerprints
// and pg_dump, so concurrent metric/session/operation updates do not corrupt
// the comparison. The fresh restore uses the same Postgres host in this lab.
func Run(ctx context.Context, options Options) (report Report, runErr error) {
	report = Report{Result: "running", Stage: "preflight", StartedAt: time.Now().UTC(),
		Limitations: []string{"same PostgreSQL instance and failure domain", "no PostgreSQL global roles/ACL restoration", "no workers started against restore", "no K8s Secrets/HMAC/routes or Neon application-data restore", "no HA failover or RPO/RTO certification"}}
	workspace, err := filepath.Abs(options.Workspace)
	if err != nil {
		return report, err
	}
	private, err := filepath.Abs(options.PrivateDirectory)
	if err != nil {
		return report, err
	}
	evidence, err := filepath.Abs(options.EvidenceFile)
	if err != nil {
		return report, err
	}
	if !within(private, filepath.Join(workspace, "control-plane", ".local")) || !within(evidence, filepath.Join(workspace, "control-plane", "production", "evidence")) {
		return report, errors.New("require private backup and public evidence locations within workspace")
	}
	if _, err = os.Stat(private); !os.IsNotExist(err) {
		return report, errors.New("private attempt already exists or cannot be checked")
	}
	if err = os.MkdirAll(filepath.Dir(evidence), 0700); err != nil {
		return report, err
	}
	evidenceHandle, err := os.OpenFile(evidence, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return report, err
	}
	defer evidenceHandle.Close()
	save := func() error {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err = evidenceHandle.Truncate(0); err != nil {
			return err
		}
		if _, err = evidenceHandle.Seek(0, io.SeekStart); err != nil {
			return err
		}
		_, err = evidenceHandle.Write(append(data, '\n'))
		return err
	}
	defer func() {
		now := time.Now().UTC()
		report.FinishedAt = &now
		if runErr != nil {
			report.Result = "fail"
			// Full diagnostics may include SQL/database details. Keep them only
			// in the new private attempt, never in terminal or public evidence.
			if handle, err := os.OpenFile(filepath.Join(private, "failure.private.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600); err == nil {
				handle.Write([]byte(runErr.Error() + "\n"))
				handle.Close()
			}
		}
		if err := save(); err != nil && runErr == nil {
			runErr = err
		}
	}()
	if err = os.Mkdir(private, 0700); err != nil {
		return report, err
	}
	if err = save(); err != nil {
		return report, err
	}
	config, err := pgx.ParseConfig(options.DatabaseURL)
	if err != nil {
		return report, errors.New("invalid private database config")
	}
	if config.Database != "neon_control_v2" || config.User != "neon_control_v2" {
		return report, errors.New("unexpected source database or owner")
	}
	config.RuntimeParams["timezone"] = "UTC"
	config.RuntimeParams["datestyle"] = "ISO, MDY"
	source, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		return report, err
	}
	defer source.Close(context.Background())
	tx, err := source.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(context.Background())
	var snapshot string
	if err = tx.QueryRow(ctx, "SELECT pg_export_snapshot()").Scan(&snapshot); err != nil {
		return report, err
	}
	if !regexp.MustCompile(`^[0-9A-Fa-f-]+$`).MatchString(snapshot) {
		return report, errors.New("unexpected exported snapshot")
	}
	report.Stage = "snapshot-fingerprint"
	if report.SourceTables, err = fingerprint(ctx, tx); err != nil {
		return report, err
	}
	if err = save(); err != nil {
		return report, err
	}
	report.Stage = "ssh-config"
	callback, err := knownhosts.New(filepath.Join(workspace, ".ssh-known-hosts"))
	if err != nil {
		return report, err
	}
	address := "192.168.146.101:22"
	report.Stage = "ssh-connect"
	network, err := net.DialTimeout("tcp", address, 20*time.Second)
	if err != nil {
		return report, err
	}
	network.SetDeadline(time.Now().Add(20 * time.Second))
	report.Stage = "ssh-handshake"
	knownData, err := os.ReadFile(filepath.Join(workspace, ".ssh-known-hosts"))
	if err != nil {
		network.Close()
		return report, err
	}
	algorithms, err := trustedHostKeyAlgorithms(knownData, address, network.RemoteAddr(), callback)
	if err != nil {
		network.Close()
		return report, err
	}
	connection, channels, requests, err := ssh.NewClientConn(network, address, &ssh.ClientConfig{User: "root", Auth: []ssh.AuthMethod{ssh.Password(options.SSHPassword)}, HostKeyCallback: callback, HostKeyAlgorithms: algorithms})
	if err != nil {
		network.Close()
		return report, err
	}
	network.SetDeadline(time.Time{})
	client := ssh.NewClient(connection, channels, requests)
	defer client.Close()
	log, err := os.OpenFile(filepath.Join(private, "postgres-tools.stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return report, err
	}
	defer log.Close()
	// Discovery is bounded and contains no Secret fields.
	report.Stage = "database-pod-discovery"
	var podJSON strings.Builder
	kubectl := "KUBECONFIG=/etc/rancher/rke2/rke2.yaml /var/lib/rancher/rke2/bin/kubectl --server=https://192.168.146.101:6443 -n neon"
	if err = remote(ctx, client, kubectl+" get pods -l app.kubernetes.io/name=neon-control-v2-db -o json", nil, &podJSON, log); err != nil {
		return report, err
	}
	var pods struct {
		Items []struct {
			Metadata struct{ Name, UID string }
			Status   struct{ Phase string }
		}
	}
	if err = json.Unmarshal([]byte(podJSON.String()), &pods); err != nil {
		return report, err
	}
	if len(pods.Items) != 1 || pods.Items[0].Status.Phase != "Running" {
		return report, errors.New("expected one running control database Pod")
	}
	if !regexp.MustCompile(`^[a-z0-9-]+$`).MatchString(pods.Items[0].Metadata.Name) {
		return report, errors.New("invalid database Pod name")
	}
	report.SourcePodUID = pods.Items[0].Metadata.UID
	exec := kubectl + " exec -i " + shellQuote(pods.Items[0].Metadata.Name) + " -- sh -c "
	report.Stage = "dump-exported-snapshot"
	archivePath := filepath.Join(private, "neon_control_v2.dump")
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return report, err
	}
	tool := `PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -U neon_control_v2 -d neon_control_v2 --format=custom --snapshot=` + shellQuote(snapshot)
	err = remote(ctx, client, exec+shellQuote(tool), nil, archive, log)
	closeErr := archive.Close()
	if err != nil {
		return report, err
	}
	if closeErr != nil {
		return report, closeErr
	}
	if err = tx.Commit(ctx); err != nil {
		return report, err
	}
	archiveRead, err := os.Open(archivePath)
	if err != nil {
		return report, err
	}
	defer archiveRead.Close()
	magic := make([]byte, 5)
	if _, err = io.ReadFull(archiveRead, magic); err != nil || string(magic) != "PGDMP" {
		return report, errors.New("invalid custom archive")
	}
	archiveRead.Seek(0, io.SeekStart)
	hash := sha256.New()
	if report.DumpBytes, err = io.Copy(hash, archiveRead); err != nil {
		return report, err
	}
	report.DumpSHA256 = hex.EncodeToString(hash.Sum(nil))
	archiveRead.Seek(0, io.SeekStart)
	var nonce [6]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return report, err
	}
	report.TargetDatabase = "neon_dr_" + time.Now().UTC().Format("20060102t150405") + "_" + hex.EncodeToString(nonce[:])
	if !regexp.MustCompile(`^neon_dr_[a-z0-9_]+$`).MatchString(report.TargetDatabase) {
		return report, errors.New("invalid restore database")
	}
	report.Stage = "create-isolated-database"
	if err = save(); err != nil {
		return report, err
	}
	if _, err = source.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{report.TargetDatabase}.Sanitize()+" TEMPLATE template0 OWNER neon_control_v2"); err != nil {
		return report, err
	}
	if _, err = source.Exec(ctx, "REVOKE CONNECT ON DATABASE "+pgx.Identifier{report.TargetDatabase}.Sanitize()+" FROM PUBLIC"); err != nil {
		return report, err
	}
	report.Stage = "restore-single-transaction"
	if err = save(); err != nil {
		return report, err
	}
	tool = `PGPASSWORD="$POSTGRES_PASSWORD" pg_restore -U neon_control_v2 --exit-on-error --single-transaction --no-owner --no-privileges --dbname=` + shellQuote(report.TargetDatabase)
	if err = remote(ctx, client, exec+shellQuote(tool), archiveRead, io.Discard, log); err != nil {
		return report, err
	}
	report.ArchiveRestored = true
	targetConfig := config.Copy()
	targetConfig.Database = report.TargetDatabase
	target, err := pgx.ConnectConfig(ctx, targetConfig)
	if err != nil {
		return report, err
	}
	defer target.Close(context.Background())
	targetTx, err := target.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return report, err
	}
	defer targetTx.Rollback(context.Background())
	report.Stage = "verify-restored-tables"
	if report.RestoredTables, err = fingerprint(ctx, targetTx); err != nil {
		return report, err
	}
	report.TablesEqual = reflect.DeepEqual(report.SourceTables, report.RestoredTables)
	if !report.TablesEqual {
		return report, errors.New("restored table content differs from exported snapshot")
	}
	var invalid int
	if err = targetTx.QueryRow(ctx, `SELECT count(*) FROM pg_constraint c JOIN pg_namespace n ON n.oid=c.connamespace WHERE NOT c.convalidated AND n.nspname NOT LIKE 'pg\_%' AND n.nspname<>'information_schema'`).Scan(&invalid); err != nil {
		return report, err
	}
	report.ConstraintsValid = invalid == 0
	if !report.ConstraintsValid {
		return report, errors.New("restore contains unvalidated constraints")
	}
	if err = targetTx.Commit(ctx); err != nil {
		return report, err
	}
	report.Result = "pass"
	report.Stage = "retained-isolated-restore-verified"
	return report, nil
}
