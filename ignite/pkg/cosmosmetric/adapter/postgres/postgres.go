package postgres

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"

	"github.com/ignite-hq/cli/ignite/pkg/cosmosclient"

	_ "github.com/lib/pq" // required to register postgres sql driver
)

const (
	adapterType = "postgres"

	defaultPort = 5432
	defaultHost = "127.0.0.1"

	queryBlockHeight = `
		SELECT MAX(height)
		FROM tx
	`
	queryInsertTX = `
		INSERT INTO tx (hash, index, height, block_time)
		VALUES ($1, $2, $3, $4)
	`
	queryInsertAttr = `
		INSERT INTO attribute (tx_hash, event_type, event_index, name, value)
		VALUES ($1, $2, $3, $4, $5)
	`
	querySchemaExists = `
		SELECT EXISTS (
			SELECT FROM information_schema.tables
			WHERE table_schema = 'public' AND table_name = 'schema'
		)
	`
	querySchemaVersion = `
		SELECT MAX(version)
		FROM schema
	`

	// Latest schema version that the adapter should apply.
	// This version should be updated when new schema/*.sql files are added
	// to match the name of the latest file, otherwise the new schemas won't
	// be applied. All schema file names MUST be numeric.
	schemaVersion = 1
)

//go:embed schemas/*
var fsSchemas embed.FS

var (
	// ErrClosed is returned when database connection is not open.
	ErrClosed = errors.New("no database connection")
)

// Option defines an option for the adapter.
type Option func(*Adapter)

// WithHost configures a database host name or IP.
func WithHost(host string) Option {
	return func(a *Adapter) {
		a.host = host
	}
}

// WithPort configures a database port.
func WithPort(port uint) Option {
	return func(a *Adapter) {
		a.port = port
	}
}

// WithUser configures a database user.
func WithUser(user string) Option {
	return func(a *Adapter) {
		a.user = user
	}
}

// WithPassword configures a database password.
func WithPassword(password string) Option {
	return func(a *Adapter) {
		a.password = password
	}
}

// WithParams configures extra database parameters.
func WithParams(params map[string]string) Option {
	return func(a *Adapter) {
		a.params = params
	}
}

// NewAdapter creates a new PostgreSQL adapter.
func NewAdapter(database string, options ...Option) (Adapter, error) {
	adapter := Adapter{
		host: defaultHost,
		port: defaultPort,
	}

	for _, o := range options {
		o(&adapter)
	}

	db, err := sql.Open("postgres", createPostgresURI(adapter))
	if err != nil {
		return Adapter{}, err
	}

	adapter.db = db

	return adapter, nil
}

// Adapter implements a data backend adapter for PostgreSQL.
type Adapter struct {
	host, user, password, database string
	port                           uint
	params                         map[string]string

	db *sql.DB
}

func (a Adapter) GetType() string {
	return adapterType
}

func (a Adapter) SetupSchema(ctx context.Context) error {
	current, err := a.getSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("failed to read schema version: %w", err)
	}

	if current == schemaVersion {
		return nil
	} else if current > schemaVersion {
		return fmt.Errorf("latest schema version is v%d, found v%d", schemaVersion, current)
	}

	for i := current + 1; i <= schemaVersion; i++ {
		name := fmt.Sprintf("%d.sql", i)
		if err := a.applySchema(ctx, name); err != nil {
			return fmt.Errorf("error applying schema %s: %w", name, err)
		}
	}

	return nil
}

// TODO: add support to save raw transaction data
func (a Adapter) Save(ctx context.Context, txs []cosmosclient.TX) error {
	db, err := a.getDB()
	if err != nil {
		return err
	}

	// Start a transaction
	sqlTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	// Note: rollback won't have any effect if the transaction is committed before
	defer sqlTx.Rollback()

	// Prepare insert statements to speed up "bulk" saving times
	txStmt, err := sqlTx.PrepareContext(ctx, queryInsertTX)
	if err != nil {
		return err
	}

	defer txStmt.Close()

	attrStmt, err := sqlTx.PrepareContext(ctx, queryInsertAttr)
	if err != nil {
		return err
	}

	defer attrStmt.Close()

	// Save the transactions and event attributes
	for _, tx := range txs {
		hash := tx.Raw.Hash.String()
		if _, err := txStmt.ExecContext(ctx, hash, tx.Raw.Index, tx.Raw.Height, tx.BlockTime); err != nil {
			return fmt.Errorf("error saving transaction %s: %w", hash, err)
		}

		events, err := cosmosclient.UnmarshallEvents(tx)
		if err != nil {
			return err
		}

		for i, evt := range events {
			for _, attr := range evt.Attributes {
				// The attribute value must be saved as a JSON encoded value
				v, err := json.Marshal(attr.Value)
				if err != nil {
					return fmt.Errorf("failed to encode event attribute '%s': %w", attr.Key, err)
				}

				if _, err := attrStmt.ExecContext(ctx, hash, evt.Type, i, attr.Key, v); err != nil {
					return fmt.Errorf("error saving event attribute: %w", err)
				}
			}
		}
	}

	return sqlTx.Commit()
}

func (a Adapter) GetLatestHeight(ctx context.Context) (height int64, err error) {
	db, err := a.getDB()
	if err != nil {
		return 0, err
	}

	row := db.QueryRowContext(ctx, queryBlockHeight)
	if err = row.Scan(&height); err != nil {
		return 0, err
	}

	return height, nil
}

func (a Adapter) getDB() (*sql.DB, error) {
	if a.db == nil {
		return nil, ErrClosed
	}

	return a.db, nil
}

func (a Adapter) getSchemaVersion(ctx context.Context) (version uint, err error) {
	db, err := a.getDB()
	if err != nil {
		return 0, err
	}

	exists := false
	row := db.QueryRowContext(ctx, querySchemaExists)
	if err = row.Scan(&exists); err != nil {
		return 0, err
	}

	if !exists {
		return 0, nil
	}

	row = db.QueryRowContext(ctx, querySchemaVersion)
	if err = row.Scan(&version); err != nil {
		return 0, err
	}

	return version, nil
}

func (a Adapter) applySchema(ctx context.Context, filename string) error {
	script, err := fsSchemas.ReadFile(fmt.Sprintf("schemas/%s", filename))
	if err != nil {
		return err
	}

	db, err := a.getDB()
	if err != nil {
		return err
	}

	_, err = db.ExecContext(ctx, string(script))

	return err
}

func createPostgresURI(a Adapter) string {
	uri := url.URL{
		Scheme: adapterType,
		Host:   fmt.Sprintf("%s:%d", a.host, a.port),
		Path:   a.database,
	}

	if a.user != "" {
		if a.password != "" {
			uri.User = url.UserPassword(a.user, a.password)
		} else {
			uri.User = url.User(a.user)
		}
	}

	// Add extra params as query arguments
	if a.params != nil {
		query := url.Values{}
		for k, v := range a.params {
			query.Set(k, v)
		}

		uri.RawQuery = query.Encode()
	}

	return uri.String()
}