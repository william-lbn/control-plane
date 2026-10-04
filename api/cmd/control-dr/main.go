package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/william-lbn/control-plane/api/internal/recovery"
)

func main() {
	var options recovery.Options
	flag.StringVar(&options.Workspace, "workspace", "", "Absolute workspace root")
	flag.StringVar(&options.PrivateDirectory, "private-dir", "", "Fresh directory under control-plane/.local")
	flag.StringVar(&options.EvidenceFile, "evidence", "", "Fresh public evidence filename")
	flag.Parse()
	options.DatabaseURL = os.Getenv("NEON_V2_DATABASE_URL")
	options.SSHPassword = os.Getenv("NEON_LAB_SSH_PASSWORD")
	if options.Workspace == "" || options.PrivateDirectory == "" || options.EvidenceFile == "" || options.DatabaseURL == "" || options.SSHPassword == "" {
		fmt.Fprintln(os.Stderr, "Required paths or private environment credentials missing")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	report, err := recovery.Run(ctx, options)
	json.NewEncoder(os.Stdout).Encode(map[string]any{"result": report.Result, "stage": report.Stage, "tables": len(report.SourceTables), "archive_restored": report.ArchiveRestored, "complete_system_dr": false})
	if err != nil {
		// Driver errors may contain database authentication details or restored
		// SQL; keep terminal/public output to the safe stage and error type.
		fmt.Fprintf(os.Stderr, "Recovery rehearsal failed: stage=%s error_type=%T\n", report.Stage, err)
		os.Exit(1)
	}
}
