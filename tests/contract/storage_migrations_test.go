package contract_test

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommittedStorageMigrationsMatchSpecDDL(t *testing.T) {
	files := map[string]string{
		"catalog/001.sql": "MCE4D_DDL_CATALOG_001",
		"index/001.sql":   "MCE4D_DDL_INDEX_001",
		"index/002.sql":   "MCE4D_DDL_INDEX_002",
		"index/003.sql":   "MCE4D_DDL_INDEX_003",
		"index/004.sql":   "MCE4D_DDL_INDEX_004",
		"records/001.sql": "MCE4D_DDL_RECORDS_001",
	}
	root := storageRepoRoot(t)
	for path, marker := range files {
		t.Run(path, func(t *testing.T) {
			migrationPath := filepath.Join(root, "internal", "store", "sqlite", "migrations", filepath.FromSlash(path))
			actual, err := os.ReadFile(migrationPath)
			if err != nil {
				t.Fatalf("read committed migration: %v", err)
			}
			if string(actual) != ddlFromSpec(t, marker) {
				t.Fatalf("migration %s differs from frozen SPEC block %s", path, marker)
			}
		})
	}

	for _, futurePath := range []string{
		"records/002.sql",
	} {
		path := filepath.Join(root, "internal", "store", "sqlite", "migrations", filepath.FromSlash(futurePath))
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("future schema block must stay out of shipped baseline migrations: %s (stat err=%v)", futurePath, err)
		}
	}
}
