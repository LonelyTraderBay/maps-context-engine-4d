// Command compare validates the offline synthetic benchmark suite and imports
// measurements collected by a separately authorized host runner.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/LonelyTraderBay/maps-context-engine-4d/tools/bench/compare"
)

const maxUsageFileBytes = 128 << 20

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "compare:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New("command required: validate or import")
	}
	switch args[0] {
	case "validate":
		return runValidate(args[1:], stdout, stderr)
	case "import":
		return runImport(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(stdout, "usage: compare validate -suite <corpus.json> | compare import -suite <corpus.json> -usage <usage.jsonl> -out <report.json>")
		return err
	default:
		return fmt.Errorf("unknown command %q; use validate or import", args[0])
	}
}

func runValidate(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("validate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	suitePath := flags.String("suite", "", "suite corpus JSON path")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *suitePath == "" {
		return errors.New("validate requires -suite <corpus.json>")
	}
	suite, err := compare.LoadSuite(*suitePath)
	if err != nil {
		return err
	}
	result := struct {
		Status       string `json:"status"`
		SuiteID      string `json:"suite_id"`
		Synthetic    bool   `json:"synthetic"`
		Repositories int    `json:"repositories"`
		Tasks        int    `json:"tasks"`
		Recommended  int    `json:"recommended_repeats"`
		SuiteSHA256  string `json:"suite_sha256"`
	}{"PASS", suite.SuiteID, suite.Synthetic, len(suite.Repositories), len(suite.Tasks), suite.RecommendedRuns, suite.SourceSHA256}
	return writeJSON(stdout, result)
}

func runImport(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	suitePath := flags.String("suite", "", "suite corpus JSON path")
	usagePath := flags.String("usage", "", "host-supplied usage JSONL path")
	outPath := flags.String("out", "", "new report JSON path; existing files are not overwritten")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *suitePath == "" || *usagePath == "" || *outPath == "" {
		return errors.New("import requires -suite, -usage, and -out")
	}
	suite, err := compare.LoadSuite(*suitePath)
	if err != nil {
		return err
	}
	usage, err := readBounded(*usagePath, maxUsageFileBytes)
	if err != nil {
		return err
	}
	report, err := compare.ImportUsage(suite, filepath.Dir(*suitePath), usage)
	if err != nil {
		return err
	}
	if err := writeNewJSON(*outPath, report); err != nil {
		return err
	}
	return writeJSON(stdout, struct {
		Status               string `json:"status"`
		Output               string `json:"output"`
		TokenTelemetryStatus string `json:"token_telemetry_status"`
		ExpectedTaskRuns     int    `json:"expected_task_runs"`
		ComparableTaskRuns   int    `json:"comparable_task_runs"`
	}{"PASS", *outPath, report.TokenTelemetryStatus, report.Coverage.ExpectedTaskRuns, report.Coverage.ComparableTaskRuns})
}

func readBounded(path string, maximum int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open usage file: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read usage file: %w", err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("usage file exceeds %d bytes", maximum)
	}
	return data, nil
}

func writeNewJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode report: %w", err)
	}
	data = append(data, '\n')
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create report without overwriting existing file: %w", err)
	}
	complete := false
	defer func() {
		_ = file.Close()
		if !complete {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync report: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close report: %w", err)
	}
	complete = true
	return nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write command result: %w", err)
	}
	return nil
}
