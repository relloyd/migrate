// +build go1.9

package snowflake

import (
	"database/sql"
	"fmt"
	"io"
	"io/ioutil"
	nurl "net/url"
	"regexp"
	"strings"

	"context"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database"
	snowflake "github.com/snowflakedb/gosnowflake"
)

func init() {
	db := Snowflake{}
	database.Register("snowflake", &db)
}

var DefaultMigrationsTable = "SCHEMA_MIGRATIONS"

var (
	ErrNilConfig      = fmt.Errorf("no config")
	ErrNoDatabaseName = fmt.Errorf("no database name")
	ErrNoSchema       = fmt.Errorf("no schema")
	ErrDatabaseDirty  = fmt.Errorf("database is dirty")
)

type Config struct {
	MigrationsTable string
	DatabaseName    string
}

type Snowflake struct {
	// Locking and unlocking need to use the same connection
	conn     *sql.Conn
	isLocked bool

	// Open and WithInstance need to garantuee that config is never nil
	config *Config
}

func WithInstance(instance *sql.DB, config *Config) (database.Driver, error) {
	if config == nil {
		return nil, ErrNilConfig
	}

	if err := instance.Ping(); err != nil {
		return nil, err
	}

	query := `SELECT CURRENT_DATABASE()`
	var databaseName string
	if err := instance.QueryRow(query).Scan(&databaseName); err != nil {
		return nil, &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if len(databaseName) == 0 {
		return nil, ErrNoDatabaseName
	}

	config.DatabaseName = databaseName

	if len(config.MigrationsTable) == 0 {
		config.MigrationsTable = DefaultMigrationsTable
	}

	conn, err := instance.Conn(context.Background())

	if err != nil {
		return nil, err
	}

	sx := &Snowflake{
		conn:   conn,
		config: config,
	}

	if err := sx.ensureVersionTable(); err != nil {
		return nil, err
	}

	return sx, nil
}

func (s *Snowflake) Open(url string) (database.Driver, error) {
	purl, err := nurl.Parse(url)
	if err != nil {
		return nil, err
	}

	purl.Scheme = ""
	db, err := sql.Open("snowflake", strings.TrimPrefix(migrate.FilterCustomQuery(purl).String(), "//"))
	if err != nil {
		return nil, err
	}

	migrationsTable := purl.Query().Get("x-migrations-table")
	if len(migrationsTable) == 0 {
		migrationsTable = DefaultMigrationsTable
	}

	sx, err := WithInstance(db, &Config{
		DatabaseName:    purl.Path,
		MigrationsTable: migrationsTable,
	})
	if err != nil {
		return nil, err
	}

	return sx, nil
}

func (s *Snowflake) Close() error {
	return s.conn.Close()
}

// Lock implements the database.Driver interface by not locking and returning nil.
func (s *Snowflake) Lock() error { return nil }

// Unlock implements the database.Driver interface by not locking and returning nil.
func (s *Snowflake) Unlock() error { return nil }

func (s *Snowflake) Run(migration io.Reader) error {
	migr, err := ioutil.ReadAll(migration)
	if err != nil {
		return err
	}
	terminatorPattern := ";\r{0,1}\n"
	commentLinePattern := "--.*;\r{0,1}\n"
	reCommentLine := regexp.MustCompile(commentLinePattern)
	queries := regexpSplit(string(migr[:]), terminatorPattern) // split the input file into separate queries.
	for _, query := range queries { // for each SQL statement in the input text...
		if (strings.HasPrefix(query, "--") && reCommentLine.Match([]byte(query))) ||
			len(query) == 0	{
			// if the query starts with a comment and is just a terminated comment
			// OR the query is empty...
			continue // skip this!
		}
		if _, err := s.conn.ExecContext(context.Background(), query); err != nil { // if execution failed...
			return database.Error{OrigErr: err, Err: "migration failed", Query: migr}
		}
	}
	return nil
}

func (s *Snowflake) SetVersion(version int, dirty bool) error {
	tx, err := s.conn.BeginTx(context.Background(), &sql.TxOptions{})
	if err != nil {
		return &database.Error{OrigErr: err, Err: "transaction start failed"}
	}

	query := `TRUNCATE "` + s.config.MigrationsTable + `"`
	if _, err := tx.Exec(query); err != nil {
		tx.Rollback()
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}

	if version >= 0 {
		query = `INSERT INTO "` + s.config.MigrationsTable + `" (version, dirty) VALUES (?, ?)`
		if _, err := tx.Exec(query, version, dirty); err != nil {
			tx.Rollback()
			return &database.Error{OrigErr: err, Query: []byte(query)}
		}
	}

	if err := tx.Commit(); err != nil {
		return &database.Error{OrigErr: err, Err: "transaction commit failed"}
	}

	return nil
}

func (s *Snowflake) Version() (version int, dirty bool, err error) {
	query := `SELECT version, dirty FROM "` + s.config.MigrationsTable + `" LIMIT 1`
	err = s.conn.QueryRowContext(context.Background(), query).Scan(&version, &dirty)
	switch {
	case err == sql.ErrNoRows:
		return database.NilVersion, false, nil

	case err != nil:
		if e, ok := err.(*snowflake.SnowflakeError); ok {
			if e.Number == snowflake.ErrCodeObjectNotExists {
				return database.NilVersion, false, nil
			}
		}
		return 0, false, &database.Error{OrigErr: err, Query: []byte(query)}

	default:
		return version, dirty, nil
	}
}

func (s *Snowflake) Drop() error {
	// select all tables in current schema
	query := `SELECT table_name FROM information_schema.tables WHERE table_schema=(SELECT current_schema())`
	tables, err := s.conn.QueryContext(context.Background(), query)
	if err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	defer tables.Close()

	// delete one table after another
	tableNames := make([]string, 0)
	for tables.Next() {
		var tableName string
		if err := tables.Scan(&tableName); err != nil {
			return err
		}
		if len(tableName) > 0 {
			tableNames = append(tableNames, tableName)
		}
	}

	if len(tableNames) > 0 {
		// delete one by one ...
		for _, t := range tableNames {
			query = `DROP TABLE IF EXISTS ` + t + ` CASCADE`
			if _, err := s.conn.ExecContext(context.Background(), query); err != nil {
				return &database.Error{OrigErr: err, Query: []byte(query)}
			}
		}
		if err := s.ensureVersionTable(); err != nil {
			return err
		}
	}

	return nil
}

func (s *Snowflake) ensureVersionTable() error {
	// check if migration table exists
	var count int
	query := `SELECT COUNT(1) FROM information_schema.tables WHERE table_name = ? AND table_schema = (SELECT current_schema()) LIMIT 1`
	if err := s.conn.QueryRowContext(context.Background(), query, s.config.MigrationsTable).Scan(&count); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	if count == 1 {
		return nil
	}

	// if not, create the empty migration table
	query = `CREATE TABLE "` + s.config.MigrationsTable + `" (version bigint not null primary key, dirty boolean not null)`
	if _, err := s.conn.ExecContext(context.Background(), query); err != nil {
		return &database.Error{OrigErr: err, Query: []byte(query)}
	}
	return nil
}

// regexpSplit splits text using the supplied regexp delimiter.
// If there is no match then the original text is returned.
// Each string in the returned slice includes the delimiter.
func regexpSplit(text string, delimiterRegexp string) []string {
	reg := regexp.MustCompile(delimiterRegexp)
	indexes := reg.FindAllStringIndex(text, -1)
	if indexes == nil { // if there was no match...
		return []string{text} // return the input text in a slice.
	}
	lastStart := 0
	result := make([]string, len(indexes)) // add 1 more if you want the trailing text that is not terminated.
	for idx, element := range indexes { // for each matched index position in the text...
		// element is an array with two values: pos 0 = start of delimiter; pos 1 = end of delimiter.
		result[idx] = text[lastStart:element[1]] // cut from lastStart to the end of the match position (include the delimiter).
		lastStart = element[1]                   // reset lastStart to just after the match string.
	}
	// Discard any trailing text that is not terminated by the delimiter.
	// This is the last substring to the end of the text: // result[len(indexes)] = text[lastStart:]
	return result
}
