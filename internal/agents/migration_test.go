package agents_test

import (
	"io/fs"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/ws4charlie/agent-gate/migrations"
)

const simpleProtocol = pgx.QueryExecModeSimpleProtocol

func readMigration(t *testing.T) string {
	t.Helper()
	b, err := fs.ReadFile(migrations.FS, "0005_policy.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
