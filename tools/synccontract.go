//go:build ignore

// Command synccontract refreshes the vendored server contract snapshot.
//
// The snapshot keeps the test suite independent of sibling repositories. Point
// FINGERLY_CONTRACT_DIR at the integration workspace's contract directory to
// pull a newer one:
//
//	FINGERLY_CONTRACT_DIR=../../contract go run tools/synccontract.go
//
// TestContractSnapshotMatchesItsManifest verifies the result, so there is no
// separate --check mode: `go test` is the check.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

var files = []string{"openapi.json", "fixtures.json"}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	source := os.Getenv("FINGERLY_CONTRACT_DIR")
	if source == "" {
		source = "contract"
	}

	manifestPath := filepath.Join(source, "manifest.json")
	manifestRaw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest struct {
		Files map[string]string `json:"files"`
	}
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		return fmt.Errorf("decode manifest: %w", err)
	}

	// Copy only after every file matches the hash the server recorded, so a
	// half-written snapshot cannot land.
	staged := map[string][]byte{"manifest.json": manifestRaw}
	for _, name := range files {
		content, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		sum := sha256.Sum256(content)
		if got := hex.EncodeToString(sum[:]); got != manifest.Files[name] {
			return fmt.Errorf("invalid contract hash for %s: %s", name, got)
		}
		staged[name] = content
	}

	if err := os.MkdirAll("contract", 0o755); err != nil {
		return err
	}
	for name, content := range staged {
		if err := os.WriteFile(filepath.Join("contract", name), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	fmt.Println("Go contract snapshot refreshed.")
	return nil
}
