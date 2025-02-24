// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package pgdb

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/jackc/pgconn"
	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgtype/pgxtype"
	"github.com/jackc/pgx/v4"
	"golang.org/x/exp/slices"

	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
)

var (
	// Regex validateCollectionNameRe validates collection names.
	validateCollectionNameRe = regexp.MustCompile("^[a-zA-Z_][a-zA-Z0-9_]{0,119}$")

	// Regex validateDatabaseNameRe validates database names.
	validateDatabaseNameRe = regexp.MustCompile("^[a-z_][a-z0-9_]{0,62}$")
)

// Collections returns a sorted list of FerretDB collection names.
//
// It returns (possibly wrapped) ErrSchemaNotExist if FerretDB database / PostgreSQL schema does not exist.
func Collections(ctx context.Context, querier pgxtype.Querier, db string) ([]string, error) {
	schemaExists, err := schemaExists(ctx, querier, db)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if !schemaExists {
		return nil, ErrSchemaNotExist
	}

	settings, err := getSettingsTable(ctx, querier, db)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	collectionsDoc := must.NotFail(settings.Get("collections"))

	collections, ok := collectionsDoc.(*types.Document)
	if !ok {
		return nil, lazyerrors.Errorf("invalid settings document: %v", collectionsDoc)
	}

	// TODO sort collections on update
	names := collections.Keys()
	slices.Sort(names)

	return names, nil
}

// CollectionExists returns true if FerretDB collection exists.
func CollectionExists(ctx context.Context, querier pgxtype.Querier, db, collection string) (bool, error) {
	collections, err := Collections(ctx, querier, db)
	if err != nil {
		if errors.Is(err, ErrSchemaNotExist) {
			return false, nil
		}
		return false, err
	}

	return slices.Contains(collections, collection), nil
}

// CreateCollection creates a new FerretDB collection in existing schema.
//
// It returns a possibly wrapped error:
//   - ErrInvalidTableName - if a FerretDB collection name doesn't conform to restrictions.
//   - ErrAlreadyExist - if a FerretDB collection with the given names already exists.
//   - ErrTableNotExist - is the required FerretDB database does not exist.
//
// Please use errors.Is to check the error.
func CreateCollection(ctx context.Context, querier pgxtype.Querier, db, collection string) error {
	if !validateCollectionNameRe.MatchString(collection) ||
		strings.HasPrefix(collection, reservedPrefix) {
		return ErrInvalidTableName
	}

	schemaExists, err := schemaExists(ctx, querier, db)
	if err != nil {
		return lazyerrors.Error(err)
	}

	if !schemaExists {
		return ErrSchemaNotExist
	}

	table := formatCollectionName(collection)
	tables, err := tables(ctx, querier, db)
	if err != nil {
		return err
	}
	if slices.Contains(tables, table) {
		return ErrAlreadyExist
	}

	settings, err := getSettingsTable(ctx, querier, db)
	if err != nil {
		return lazyerrors.Error(err)
	}

	collectionsDoc := must.NotFail(settings.Get("collections"))
	collections, ok := collectionsDoc.(*types.Document)
	if !ok {
		return lazyerrors.Errorf("expected document but got %[1]T: %[1]v", collectionsDoc)
	}

	if collections.Has(collection) {
		return nil
	}

	// TODO keep "collections" sorted after each update
	must.NoError(collections.Set(collection, table))
	must.NoError(settings.Set("collections", collections))

	err = updateSettingsTable(ctx, querier, db, settings)
	if err != nil {
		return lazyerrors.Error(err)
	}

	sql := `CREATE TABLE IF NOT EXISTS ` + pgx.Identifier{db, table}.Sanitize() + ` (_jsonb jsonb)`
	if _, err = querier.Exec(ctx, sql); err == nil {
		return nil
	}

	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return lazyerrors.Error(err)
	}

	switch pgErr.Code {
	case pgerrcode.UniqueViolation, pgerrcode.DuplicateObject, pgerrcode.DuplicateTable:
		// https://www.postgresql.org/message-id/CA+TgmoZAdYVtwBfp1FL2sMZbiHCWT4UPrzRLNnX1Nb30Ku3-gg@mail.gmail.com
		// Reproducible by integration tests.
		return ErrAlreadyExist
	default:
		return lazyerrors.Error(err)
	}
}

// CreateCollectionIfNotExist ensures that given FerretDB database / PostgreSQL schema
// and FerretDB collection / PostgreSQL table exist.
// If needed, it creates both schema and table.
//
// True is returned if table was created.
func CreateCollectionIfNotExist(ctx context.Context, querier pgxtype.Querier, db, collection string) (bool, error) {
	exists, err := CollectionExists(ctx, querier, db, collection)
	if err != nil {
		return false, lazyerrors.Error(err)
	}

	if exists {
		return false, nil
	}

	// Table (or even schema) does not exist. Try to create it,
	// but keep in mind that it can be created in concurrent connection.

	if err := CreateDatabase(ctx, querier, db); err != nil && !errors.Is(err, ErrAlreadyExist) {
		return false, lazyerrors.Error(err)
	}

	// TODO use a transaction instead of pgPool: https://github.com/FerretDB/FerretDB/issues/866
	if err := CreateCollection(ctx, querier, db, collection); err != nil {
		if errors.Is(err, ErrAlreadyExist) {
			return false, nil
		}

		return false, lazyerrors.Error(err)
	}

	return true, nil
}

// DropCollection drops FerretDB collection.
//
// It returns (possibly wrapped) ErrTableNotExist if schema or table does not exist.
// Please use errors.Is to check the error.
func DropCollection(ctx context.Context, querier pgxtype.Querier, schema, collection string) error {
	schemaExists, err := schemaExists(ctx, querier, schema)
	if err != nil {
		return lazyerrors.Error(err)
	}

	if !schemaExists {
		return ErrSchemaNotExist
	}

	table := formatCollectionName(collection)
	tables, err := tables(ctx, querier, schema)
	if err != nil {
		return lazyerrors.Error(err)
	}
	if !slices.Contains(tables, table) {
		return ErrTableNotExist
	}

	err = removeTableFromSettings(ctx, querier, schema, collection)
	if err != nil && !errors.Is(err, ErrTableNotExist) {
		return lazyerrors.Error(err)
	}
	if errors.Is(err, ErrTableNotExist) {
		return ErrTableNotExist
	}

	// TODO https://github.com/FerretDB/FerretDB/issues/811
	sql := `DROP TABLE IF EXISTS ` + pgx.Identifier{schema, table}.Sanitize() + ` CASCADE`
	_, err = querier.Exec(ctx, sql)
	if err != nil {
		return lazyerrors.Error(err)
	}

	return nil
}
