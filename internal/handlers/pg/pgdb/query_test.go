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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/must"
	"github.com/FerretDB/FerretDB/internal/util/testutil"
)

func TestQueryDocuments(t *testing.T) {
	t.Parallel()

	ctx := testutil.Ctx(t)

	pool := getPool(ctx, t, zaptest.NewLogger(t))
	dbName := testutil.DatabaseName(t)
	collectionName := testutil.CollectionName(t)

	t.Cleanup(func() {
		pool.DropDatabase(ctx, dbName)
	})

	pool.DropDatabase(ctx, dbName)
	require.NoError(t, CreateDatabase(ctx, pool, dbName))

	cases := []struct {
		name       string
		collection string
		documents  []*types.Document

		// docsPerIteration represents how many documents should be fetched per each iteration,
		// use len(docsPerIteration) as the amount of fetch iterations.
		docsPerIteration []int
	}{
		{
			name:             "empty",
			collection:       collectionName,
			documents:        []*types.Document{},
			docsPerIteration: []int{},
		},
		{
			name:             "one",
			collection:       collectionName + "_one",
			documents:        []*types.Document{must.NotFail(types.NewDocument("id", "1"))},
			docsPerIteration: []int{1},
		},
		{
			name:       "two",
			collection: collectionName + "_two",
			documents: []*types.Document{
				must.NotFail(types.NewDocument("id", "1")),
				must.NotFail(types.NewDocument("id", "2")),
			},
			docsPerIteration: []int{2},
		},
		{
			name:       "three",
			collection: collectionName + "_three",
			documents: []*types.Document{
				must.NotFail(types.NewDocument("id", "1")),
				must.NotFail(types.NewDocument("id", "2")),
				must.NotFail(types.NewDocument("id", "3")),
			},
			docsPerIteration: []int{2, 1},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tx, err := pool.Begin(ctx)
			require.NoError(t, err)

			for _, doc := range tc.documents {
				require.NoError(t, InsertDocument(ctx, tx, dbName, tc.collection, doc))
			}

			sp := SQLParam{DB: dbName, Collection: tc.collection}
			it, err := pool.QueryDocuments(ctx, tx, sp)
			require.NoError(t, err)
			defer it.Close()

			iter := 0
			for it.Next() {
				fetched, err := it.DocumentsFiltered(nil)
				assert.NoError(t, err)
				assert.Equal(t, tc.docsPerIteration[iter], len(fetched))
				iter++
			}

			assert.Equal(t, len(tc.docsPerIteration), iter)

			require.NoError(t, tx.Commit(ctx))
		})
	}

	// Special case: cancel context before reading from channel.
	t.Run("cancel_context_before", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		for i := 1; i <= QueryIteratorBufSize*QueryIteratorSliceCapacity+1; i++ {
			require.NoError(t, InsertDocument(ctx, tx, dbName, collectionName+"_cancel",
				must.NotFail(types.NewDocument("id", fmt.Sprintf("%d", i))),
			))
		}

		sp := SQLParam{DB: dbName, Collection: collectionName + "_cancel"}
		ctx, cancel := context.WithCancel(context.Background())
		iterator, err := pool.QueryDocuments(ctx, pool, sp)
		require.NoError(t, err)
		defer iterator.Close()
		cancel()

		<-ctx.Done()
		countDocs := 0
		for iterator.Next() {
			_, _ = iterator.DocumentsFiltered(nil)
			countDocs++
		}
		require.Less(t, countDocs, QueryIteratorBufSize*QueryIteratorSliceCapacity+1)

		require.ErrorIs(t, tx.Rollback(ctx), context.Canceled)
	})

	// Special case: querying a non-existing collection.
	t.Run("non-existing_collection", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		require.NoError(t, err)

		sp := SQLParam{DB: dbName, Collection: collectionName + "_non-existing"}
		it, err := pool.QueryDocuments(context.Background(), tx, sp)
		require.NoError(t, err)
		defer it.Close()
		require.False(t, it.Next())
		doc, err := it.DocumentsFiltered(nil)
		require.Empty(t, doc)
		require.NoError(t, err)

		require.NoError(t, tx.Commit(ctx))
	})
}
