package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func TestMain(m *testing.M) {
	setupLoggers()
	os.Exit(m.Run())
}

func TestCopyTableInBatchesRejectsEmptySourceColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery(`(?s)SELECT column_name.*information_schema\.columns`).
		WithArgs("public", "missing_table").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}))

	err = CopyTableInBatches(context.Background(), db, make(chan *sql.DB), "public", "missing_table", 1000)
	if err == nil || err.Error() != "source relation public.missing_table does not exist, is not visible, or has no columns" {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestCopyTableInBatchesPreflightsTarget(t *testing.T) {
	source, sourceMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	target, targetMock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()

	sourceMock.ExpectQuery(`(?s)SELECT column_name.*information_schema\.columns`).
		WithArgs("public", "metrics").
		WillReturnRows(sqlmock.NewRows([]string{"column_name"}).AddRow("id").AddRow("value"))
	sourceMock.ExpectQuery(`(?s)SELECT a\.attname.*pg_index`).
		WithArgs("public.metrics").
		WillReturnRows(sqlmock.NewRows([]string{"attname"}).AddRow("id"))
	targetMock.ExpectQuery(`SELECT id, value FROM public\.metrics LIMIT 0`).
		WillReturnError(&pq.Error{Code: "42P01", Message: `relation "public.metrics" does not exist`})

	destinations := make(chan *sql.DB, 1)
	destinations <- target
	err = CopyTableInBatches(context.Background(), source, destinations, "public", "metrics", 1000)
	if err == nil || !strings.Contains(err.Error(), `target validation failed for public.metrics: pq: relation "public.metrics" does not exist`) {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(destinations) != 1 {
		t.Fatal("target connection was not returned to the pool")
	}
	if err := sourceMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	if err := targetMock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestGetColumnNamesPropagatesRowError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	want := errors.New("connection interrupted")
	rows := sqlmock.NewRows([]string{"column_name"}).AddRow("id").RowError(0, want)
	mock.ExpectQuery(`(?s)SELECT column_name.*information_schema\.columns`).
		WithArgs("public", "metrics").
		WillReturnRows(rows)

	_, err = getColumnNames(db, "public", "metrics")
	if !errors.Is(err, want) {
		t.Fatalf("expected row error, got %v", err)
	}
}

func TestWithRetryDoesNotRetryPermanentErrors(t *testing.T) {
	previous := maxRetries
	maxRetries = 5
	t.Cleanup(func() { maxRetries = previous })

	attempts := 0
	want := &pq.Error{Code: "42P01", Message: "relation does not exist"}
	err := withRetry(context.Background(), func() error {
		attempts++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("expected one attempt, got %d", attempts)
	}
}

func TestWithRetryZeroRetriesDoesNotWait(t *testing.T) {
	previous := maxRetries
	maxRetries = 0
	t.Cleanup(func() { maxRetries = previous })

	attempts := 0
	err := withRetry(context.Background(), func() error {
		attempts++
		return io.EOF
	})
	if err == nil {
		t.Fatal("expected retry exhaustion error")
	}
	if attempts != 1 {
		t.Fatalf("expected one attempt, got %d", attempts)
	}
}

func TestIsTransientError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "connection SQLSTATE", err: &pq.Error{Code: "08006"}, want: true},
		{name: "serialization", err: &pq.Error{Code: "40001"}, want: true},
		{name: "deadlock", err: &pq.Error{Code: "40P01"}, want: true},
		{name: "authentication", err: &pq.Error{Code: "28P01"}, want: false},
		{name: "missing relation", err: &pq.Error{Code: "42P01"}, want: false},
		{name: "missing column", err: &pq.Error{Code: "42703"}, want: false},
		{name: "EOF", err: io.EOF, want: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isTransientError(test.err); got != test.want {
				t.Fatalf("isTransientError() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestValidateOptions(t *testing.T) {
	oldParallel, oldRetries, oldMbps, oldOffset := parallel, maxRetries, maxMegabitsPerSec, offset
	t.Cleanup(func() {
		parallel, maxRetries, maxMegabitsPerSec, offset = oldParallel, oldRetries, oldMbps, oldOffset
	})

	parallel, maxRetries, maxMegabitsPerSec, offset = 1, 0, 1, 0
	if err := validateOptions(1); err != nil {
		t.Fatalf("valid options rejected: %v", err)
	}

	tests := []struct {
		name      string
		batchSize int
		configure func()
	}{
		{name: "batch size", batchSize: 0, configure: func() {}},
		{name: "parallel", batchSize: 1, configure: func() { parallel = 0 }},
		{name: "retries", batchSize: 1, configure: func() { maxRetries = -1 }},
		{name: "bandwidth", batchSize: 1, configure: func() { maxMegabitsPerSec = 0 }},
		{name: "offset", batchSize: 1, configure: func() { offset = -1 }},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parallel, maxRetries, maxMegabitsPerSec, offset = 1, 0, 1, 0
			test.configure()
			if err := validateOptions(test.batchSize); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}
