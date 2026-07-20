package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"os"
	"os/signal"
	"strings"
	"time"

	_ "github.com/lib/pq"
	"golang.org/x/sync/errgroup"
)

var (
	debugLog          *log.Logger
	infoLog           = log.New(os.Stdout, "", log.LstdFlags)
	maxRetries        int
	verbose           bool
	maxMegabitsPerSec float64
	parallel          int
	offset            int
	update            bool
	skipUntilChunk    string
	errInterrupted    = errors.New("transfer interrupted")
)

// isHypertable checks whether a given table is a TimescaleDB hypertable.
func isHypertable(db *sql.DB, schema, table string) (bool, error) {
	query := `
		SELECT EXISTS (
			SELECT 1
			FROM timescaledb_information.hypertables
			WHERE hypertable_schema = $1 AND hypertable_name = $2
		);
	`
	debugLog.Printf("Checking if %s.%s is a hypertable", schema, table)
	var exists bool
	err := db.QueryRow(query, schema, table).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("failed to check hypertable existence: %w", err)
	}
	return exists, nil
}

type qualifiedTableName struct {
	Schema string
	Name   string
}

// getChunksForHypertable returns the list of chunk table names for a given hypertable.
func getChunksForHypertable(db *sql.DB, schema, table string) ([]qualifiedTableName, error) {
	query := `
		SELECT chunk_schema, chunk_name
		FROM timescaledb_information.chunks
		WHERE hypertable_schema = $1 AND hypertable_name = $2
		ORDER BY range_start ASC;
	`
	debugLog.Printf("Fetching chunks for hypertable %s.%s", schema, table)
	rows, err := db.Query(query, schema, table)
	if err != nil {
		return nil, fmt.Errorf("failed to get chunks: %w", err)
	}
	defer rows.Close()

	var chunks []qualifiedTableName
	for rows.Next() {
		var chunk qualifiedTableName
		if err := rows.Scan(&chunk.Schema, &chunk.Name); err != nil {
			return nil, fmt.Errorf("failed to scan chunk: %w", err)
		}
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

type batchDescriptor struct {
	batch        [][]interface{}
	currentTable qualifiedTableName
}

func CopyTableInBatches(ctx context.Context, srcDB *sql.DB, dstDB chan *sql.DB, schema, table string, batchSize int) error {
	cols, err := getColumnNames(srcDB, schema, table)
	if err != nil {
		return fmt.Errorf("failed to get column names: %w", err)
	}

	maxWriteBatch := 65535 / len(cols)
	writeBatchSize := batchSize
	if writeBatchSize > maxWriteBatch {
		infoLog.Printf("⚠️ Reducing batch size from %d to %d to stay within PostgreSQL's parameter limit", writeBatchSize, maxWriteBatch)
		writeBatchSize = maxWriteBatch
	}

	pkCols, err := getPrimaryKeyColumns(srcDB, schema, table)
	if err != nil {
		return fmt.Errorf("failed to get primary key columns: %w", err)
	}
	if len(pkCols) == 0 {
		return fmt.Errorf("no primary key found on table %s.%s", schema, table)
	}

	if len(pkCols) > 1 && update {
		infoLog.Printf("⚠️ Skipping ON CONFLICT DO UPDATE: composite primary key detected")
	}

	estimatedRows, err := estimateRowCount(srcDB, schema, table)
	if err != nil {
		infoLog.Printf("Could not estimate row count: %v", err)
		estimatedRows = 0
	} else {
		infoLog.Printf("Estimated number of rows: %d", estimatedRows)
	}

	hypertable, err := isHypertable(srcDB, schema, table)
	if err != nil {
		return fmt.Errorf("failed to check if table is hypertable: %w", err)
	} else {
		if hypertable {
			infoLog.Printf("Table %s is a hypertable", table)
		}
	}

	if offset > 0 && hypertable {
		infoLog.Printf("⚠️ Offset is ignored for hypertables and chunk-by-chunk reads")
	}

	htChunks := []qualifiedTableName{{
		Schema: schema,
		Name:   table,
	}}
	if hypertable {
		htChunks, err = getChunksForHypertable(srcDB, schema, table)
		if err != nil {
			return fmt.Errorf("failed to get hypertable chunks: %w", err)
		}
	}

	infoLog.Printf("Primary key: %v", pkCols)
	infoLog.Printf("Columns: %v", cols)

	skipChunkMode := skipUntilChunk != ""
	if skipChunkMode && !hypertable {
		return fmt.Errorf("skip-until-chunk is only supported for hypertables")
	}
	skipDone := !skipChunkMode

	innerCtx, cancelFunc := context.WithCancel(ctx)
	defer cancelFunc()
	batchStream := make(chan batchDescriptor, 1)
	result := errgroup.Group{}

	// This goroutine keeps reading data
	result.Go(func() error {
		// Signal the writer goroutine to exit
		defer close(batchStream)

		firstIteration := true
		whereClause := ""
		lastPK := make([]interface{}, len(pkCols))

		totalBitsRead := 0.0
		startTime := time.Now()

		nextBatch := func(currentTable qualifiedTableName) ([][]interface{}, error) {

			// Build the full query
			query := buildChunkQuery(currentTable, cols, pkCols, whereClause, batchSize)
			if firstIteration && whereClause == "" && offset > 0 {
				query += fmt.Sprintf(" OFFSET %d", offset)
				debugLog.Printf("Applying initial OFFSET %d to first query", offset)
			}
			if whereClause == "" && !firstIteration {
				return nil, errors.New("only the first iteration might not have filter")
			}
			firstIteration = false
			debugLog.Printf("Executing query: %s with args: %v", query, lastPK)

			// Run the query
			var rows *sql.Rows
			err = withRetry(func() error {
				var innerErr error
				if whereClause == "" {
					rows, innerErr = srcDB.Query(query)
				} else {
					rows, innerErr = srcDB.Query(query, lastPK...)
				}
				return innerErr
			})
			if err != nil {
				return nil, fmt.Errorf("query failed: %w", err)
			}
			defer rows.Close()

			// Collect all the rows
			batch := make([][]interface{}, 0, batchSize)
			for rows.Next() {
				row := make([]interface{}, len(cols))
				ptrs := make([]interface{}, len(cols))
				for i := range row {
					ptrs[i] = &row[i]
				}
				if err := rows.Scan(ptrs...); err != nil {
					return nil, fmt.Errorf("scan failed: %w", err)
				}
				batch = append(batch, row)
				// Rough size estimate: count string/[]byte lengths + constant for primitives
				totalBitsRead += float64(estimateRowSize(row)) * 8
			}

			// If no rows collected, return io.EOF
			if len(batch) == 0 {
				return nil, io.EOF
			}

			// Otherwise, update lastPK for next chunk
			for i, col := range pkCols {
				idx := indexOf(col, cols)
				lastPK[i] = batch[len(batch)-1][idx]
				if lastPK[i] == nil {
					return nil, fmt.Errorf("primary key column %s not found in row", col)
				}
			}
			whereClause = buildWhereClause(pkCols)

			// Throttle if needed
			elapsed := time.Since(startTime)
			bitsPerSec := totalBitsRead / elapsed.Seconds()
			if bitsPerSec > maxMegabitsPerSec*1_000_000 {
				delay := time.Duration(float64(totalBitsRead)/(float64(maxMegabitsPerSec)*1e6))*time.Second - elapsed
				if delay > 0 {
					infoLog.Printf("Throttling: sleeping %s to stay under %.2f Mbps (current rate: %.2f Mbps)", delay.Round(time.Millisecond), maxMegabitsPerSec, bitsPerSec/1e6)
					time.Sleep(delay)
				}
			}

			// And return batch
			return batch, nil
		}

		// Keep getting chunks until context cancelled
		for _, currentTable := range htChunks {
			if skipChunkMode && !skipDone {
				if currentTable.Name != skipUntilChunk {
					infoLog.Printf("⏭️ Skipping chunk %s (waiting for %s)", currentTable, skipUntilChunk)
					continue
				}
				infoLog.Printf("▶️ Resuming at chunk: %s", currentTable)
				skipDone = true
			}

			firstIteration = true
			whereClause = ""
			lastPK = make([]interface{}, len(pkCols))

			chunkErr := func(currentTable qualifiedTableName) error {
				infoLog.Printf("🔍 Processing chunk: %s", currentTable)
				for {
					batch, err := nextBatch(currentTable)
					if batch != nil {
						cd := batchDescriptor{
							batch:        batch,
							currentTable: currentTable,
						}
						select {
						case <-innerCtx.Done():
							return errInterrupted
						case batchStream <- cd:
						}
					}
					if err != nil {
						if errors.Is(err, io.EOF) {
							return nil
						}
						return err
					}
				}
			}(currentTable)
			if chunkErr != nil {
				return fmt.Errorf("failed to get chunk: %w", chunkErr)
			}
		}

		if skipChunkMode && !skipDone {
			return fmt.Errorf("chunk %q not found in hypertable chunk list", skipUntilChunk)
		}
		return nil
	})

	// This goroutine keeps writing data
	result.Go(func() error {
		// Signal the reader fnction to stop
		defer cancelFunc()

		totalCopied := 0
		startTime := time.Now()

		for batch := range batchStream {
			subBatches := splitBatch(batch.batch, writeBatchSize)
			var subErr errgroup.Group
			for _, subBatch := range subBatches {
				subErr.Go(func() error {
					var db *sql.DB
					select {
					case <-innerCtx.Done():
						return errInterrupted
					case db = <-dstDB:
						defer func() { dstDB <- db }()
					}
					// Insert always targets the hypertable even if we read from individual chunks.
					if err := insertBatch(db, schema, table, cols, pkCols, subBatch); err != nil {
						return fmt.Errorf("failed to insert subbatch: %w", err)
					}
					return nil
				})
			}
			if err := subErr.Wait(); err != nil {
				return fmt.Errorf("failed to insert batch: %w", err)
			}
			firstRow := offset + totalCopied
			totalCopied += len(batch.batch)
			if estimatedRows > 0 {
				percent := float64(offset+totalCopied) / float64(estimatedRows) * 100
				elapsed := time.Since(startTime)
				rowsPerSec := float64(totalCopied) / elapsed.Seconds()
				estimatedTotalTime := time.Duration(float64(estimatedRows-offset) / rowsPerSec * float64(time.Second))
				eta := estimatedTotalTime - elapsed

				infoLog.Printf("Copied chunk %s rows %d - %d (%.1f%%) — ETA: %s", batch.currentTable, firstRow, offset+totalCopied, percent, formatDuration(eta))
			} else {
				infoLog.Printf("Copied chunk %s rows %d - %d", batch.currentTable, firstRow, offset+totalCopied)
			}
		}
		return nil
	})

	if err := result.Wait(); err != nil {
		return fmt.Errorf("failed to copy table: %w", err)
	}
	infoLog.Print("copy complete.")
	return nil
}

func splitBatch(batch [][]interface{}, writeSize int) [][][]interface{} {
	if len(batch) <= writeSize {
		return [][][]interface{}{batch}
	}
	var batches [][][]interface{}
	for len(batch) >= writeSize {
		batches = append(batches, batch[:writeSize])
		batch = batch[writeSize:]
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	return batches
}

func estimateRowSize(row []interface{}) int {
	total := 0
	for _, val := range row {
		switch v := val.(type) {
		case nil:
			total += 1
		case string:
			total += len(v)
		case []byte:
			total += len(v)
		default:
			total += 8 // assume 8 bytes for numbers, bools, etc.
		}
	}
	return total
}

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return "under 1s"
	}
	// Round to nearest second
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second

	if h > 0 {
		return fmt.Sprintf("%dh%dm%ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

func getColumnNames(db *sql.DB, schema, table string) ([]string, error) {
	query := `
        SELECT column_name
        FROM information_schema.columns
        WHERE table_schema = $1 AND table_name = $2
        ORDER BY ordinal_position;
    `
	debugLog.Printf("Executing query: %s with args: [%s, %s]", query, schema, table)
	rows, err := db.Query(query, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		cols = append(cols, col)
	}
	return cols, nil
}

func getPrimaryKeyColumns(db *sql.DB, schema, table string) ([]string, error) {
	query := `
		SELECT a.attname
		FROM pg_index i
		JOIN unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
		WHERE i.indrelid = $1::regclass AND i.indisprimary
		ORDER BY k.ord;
    `
	fullTableName := fmt.Sprintf("%s.%s", schema, table)
	debugLog.Printf("Executing query: %s with args: [%s]", query, fullTableName)
	rows, err := db.Query(query, fullTableName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var pkCols []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		pkCols = append(pkCols, col)
	}
	return pkCols, nil
}

func estimateRowCount(db *sql.DB, schema, table string) (int, error) {
	query := `
        SELECT reltuples::bigint
        FROM pg_class c
        JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relname = $1 AND n.nspname = $2;
    `
	debugLog.Printf("Executing row estimate query: %s with args: [%s, %s]", query, table, schema)
	var estimate int
	err := db.QueryRow(query, table, schema).Scan(&estimate)
	if err != nil {
		return 0, err
	}
	return estimate, nil
}

func buildChunkQuery(currentTable qualifiedTableName, cols, pkCols []string, whereClause string, chunkSize int) string {
	colList := strings.Join(cols, ", ")
	orderClause := strings.Join(pkCols, ", ")
	return fmt.Sprintf(
		`SELECT %s FROM %s.%s %s ORDER BY %s LIMIT %d`,
		colList, currentTable.Schema, currentTable.Name, whereClause, orderClause, chunkSize,
	)
}

func buildWhereClause(pkCols []string) string {
	var conditions []string
	for i := range pkCols {
		var parts []string
		for j := 0; j < i; j++ {
			parts = append(parts, fmt.Sprintf("%s = $%d", pkCols[j], j+1))
		}
		parts = append(parts, fmt.Sprintf("%s > $%d", pkCols[i], i+1))
		conditions = append(conditions, "("+strings.Join(parts, " AND ")+")")
	}
	return "WHERE " + strings.Join(conditions, " OR ")
}

func insertBatch(db *sql.DB, schema, table string, cols, pkCols []string, batch [][]interface{}) error {
	colList := strings.Join(cols, ", ")
	valPlaceholders := make([]string, len(batch))
	args := make([]interface{}, 0, len(batch)*len(cols))
	argCounter := 1

	for i, row := range batch {
		placeholders := make([]string, len(row))
		for j := range row {
			placeholders[j] = fmt.Sprintf("$%d", argCounter)
			argCounter++
		}
		valPlaceholders[i] = fmt.Sprintf("(%s)", strings.Join(placeholders, ", "))
		args = append(args, row...)
	}

	conflictClause := "ON CONFLICT DO NOTHING"
	// Para las tablas _lastdata, permito que se use ON CONFLICT DO UPDATE
	if len(pkCols) == 1 && update {
		conflictClause = buildConflictClause(cols, pkCols)
	}
	query := fmt.Sprintf(
		`INSERT INTO %s.%s (%s) VALUES %s %s`,
		schema, table, colList, strings.Join(valPlaceholders, ", "), conflictClause,
	)

	return withRetry(func() error {
		if _, err := db.Exec(query, args...); err != nil {
			return err
		}
		return nil
	})
}

func buildConflictClause(cols, pkCols []string) string {
	var updates []string
	for _, col := range cols {
		if indexOf(col, pkCols) == -1 {
			updates = append(updates, fmt.Sprintf("%s = EXCLUDED.%s", col, col))
		}
	}
	return fmt.Sprintf("ON CONFLICT (%s) DO UPDATE SET %s",
		strings.Join(pkCols, ", "),
		strings.Join(updates, ", "),
	)
}

func withRetry(action func() error) error {
	var err error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		err = action()
		if err == nil {
			return nil
		}

		if !isTransientError(err) {
			return err
		}

		wait := time.Duration(math.Pow(2, float64(attempt))) * time.Second
		wait += time.Duration(rand.Intn(1000)) * time.Millisecond
		infoLog.Printf("Transient error: %v. Retrying in %v (attempt %d/%d)...", err, wait, attempt+1, maxRetries)
		time.Sleep(wait)
	}
	return fmt.Errorf("operation failed after %d retries: %w", maxRetries, err)
}

func isTransientError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "too many connections") ||
		strings.Contains(msg, "temporarily unavailable") ||
		strings.Contains(msg, "network")
}

func indexOf(s string, list []string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func setupLoggers() {
	if verbose {
		debugLog = log.New(os.Stderr, "[DEBUG] ", log.LstdFlags)
	} else {
		debugLog = log.New(io.Discard, "", 0) // no-op logger
	}
}

func main() {
	// Flags with descriptive example defaults
	sourceDSN := flag.String("source",
		"postgres://user:pass@localhost:5432/source_db?sslmode=disable",
		"Source PostgreSQL connection string (e.g., postgres://user:pass@localhost:5432/source_db?sslmode=disable)",
	)
	targetDSN := flag.String("target",
		"postgres://user:pass@localhost:5432/target_db?sslmode=disable",
		"Target PostgreSQL connection string (e.g., postgres://user:pass@localhost:5432/target_db?sslmode=disable)",
	)
	schema := flag.String("schema", "public", "Schema name (default: public)")
	batchSize := flag.Int("batch-size", 1000, "Batch size for chunked copy (default: 1000)")
	flag.IntVar(&maxRetries, "retries", 5, "Number of retries on transient errors (default: 5)")
	flag.BoolVar(&verbose, "verbose", false, "Enable debug logging (queries, retries, etc.)")
	flag.Float64Var(&maxMegabitsPerSec, "mbps", 10, "Maximum megabits per second to read from source (default: 10)")
	flag.IntVar(&offset, "offset", 0, "Offset for the copy (default: 0)")
	flag.IntVar(&parallel, "parallel", 10, "Number of parallel writes to dest db (default: 10)")
	flag.StringVar(&skipUntilChunk, "skip-until-chunk", "", "Skip chunks until this named chunk is found (used to resume after failure)")
	flag.BoolVar(&update, "update", false, "Do ON DUPLICATE SET instead of NOTHING")
	flag.Parse()
	setupLoggers()

	tables := flag.Args()
	if *sourceDSN == "" || *targetDSN == "" || len(tables) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	ctx, cancelFunc := context.WithCancel(context.Background())
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		defer cancelFunc()
		<-c
		infoLog.Println("Received interrupt, shutting down. Please wait...")
	}()

	// Connect to source and target
	srcDB, err := sql.Open("postgres", *sourceDSN)
	if err != nil {
		log.Fatalf("Failed to connect to source DB: %v", err)
	}
	defer srcDB.Close()

	infoLog.Printf("Using %d concurrent write threads", parallel)
	dstDB := make(chan *sql.DB, parallel)
	for i := 0; i < parallel; i++ {
		db, err := sql.Open("postgres", *targetDSN)
		if err != nil {
			log.Fatalf("Failed to connect to target DB: %v", err)
		}
		// This is deferred to the end of the function, not loop
		dstDB <- db
	}

	defer func() {
		close(dstDB)
		for db := range dstDB {
			db.Close()
		}
	}()

	// Loop through tables
	failed := false
	for _, table := range tables {
		infoLog.Printf("🚀 Starting transfer of table: %s.%s", *schema, table)
		err := CopyTableInBatches(ctx, srcDB, dstDB, *schema, table, *batchSize)
		if err != nil {
			infoLog.Printf("❌ Error copying table %s.%s: %v", *schema, table, err)
			failed = true
			continue
		}
		infoLog.Printf("✅ Completed transfer of table: %s.%s\n", *schema, table)
	}
	if failed {
		os.Exit(1)
	}
}
