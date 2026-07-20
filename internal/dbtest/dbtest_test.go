package dbtest

import (
	"context"
	"errors"
	"testing"
)

// The suffix guard is the only thing standing between a mistyped
// LW_TEST_DATABASE_URL and the development database, so it is checked before
// any connection is attempted — these cases must fail without a server.
func TestURLRefusesNonTestDatabase(t *testing.T) {
	for _, url := range []string{
		"postgres://lw:lw@localhost:5433/lw_manager?sslmode=disable",
		"postgres://lw:lw@localhost:5433/postgres?sslmode=disable",
		"postgres://lw:lw@prod.example.com:5432/lw_manager_prod",
	} {
		t.Setenv("LW_TEST_DATABASE_URL", url)
		_, err := URL(context.Background())
		if !errors.Is(err, ErrUnsafeDatabase) {
			t.Errorf("URL() with %q: error = %v, want ErrUnsafeDatabase", url, err)
		}
	}
}

func TestURLRejectsMissingDatabaseName(t *testing.T) {
	t.Setenv("LW_TEST_DATABASE_URL", "postgres://lw:lw@localhost:5433/")
	if _, err := URL(context.Background()); err == nil {
		t.Fatal("URL() accepted a url with no database name")
	}
}

// The application's own variable must not leak into tests: a developer with
// LW_DATABASE_URL exported would otherwise run the suite against real data.
func TestURLIgnoresApplicationDatabaseURL(t *testing.T) {
	t.Setenv("LW_DATABASE_URL", "postgres://lw:lw@localhost:5433/lw_manager?sslmode=disable")
	t.Setenv("LW_TEST_DATABASE_URL", "")

	name, err := databaseName(DefaultURL)
	if err != nil {
		t.Fatalf("databaseName(): %v", err)
	}
	if name != "lw_manager_test" {
		t.Errorf("default database = %q, want lw_manager_test", name)
	}
}

func TestWithDatabase(t *testing.T) {
	got, err := withDatabase("postgres://lw:lw@localhost:5433/lw_manager_test?sslmode=disable", "postgres")
	if err != nil {
		t.Fatalf("withDatabase(): %v", err)
	}
	const want = "postgres://lw:lw@localhost:5433/postgres?sslmode=disable"
	if got != want {
		t.Errorf("withDatabase() = %q, want %q", got, want)
	}
}
