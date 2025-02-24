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

package tigris

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/tigrisdata/tigris-client-go/driver"

	"github.com/FerretDB/FerretDB/internal/handlers/common"
	"github.com/FerretDB/FerretDB/internal/handlers/tigris/tigrisdb"
	"github.com/FerretDB/FerretDB/internal/tjson"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
	"github.com/FerretDB/FerretDB/internal/wire"
)

// MsgDelete implements HandlerInterface.
func (h *Handler) MsgDelete(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := msg.Document()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	common.Ignored(document, h.L, "comment") // TODO https://github.com/FerretDB/FerretDB/issues/849
	if err := common.Unimplemented(document, "let"); err != nil {
		return nil, err
	}
	common.Ignored(document, h.L, "ordered") // TODO https://github.com/FerretDB/FerretDB/issues/848
	common.Ignored(document, h.L, "writeConcern")

	var deletes *types.Array
	if deletes, err = common.GetOptionalParam(document, "deletes", deletes); err != nil {
		return nil, err
	}

	ordered := true
	if ordered, err = common.GetOptionalParam(document, "ordered", ordered); err != nil {
		return nil, err
	}

	var deleted int32
	processQuery := func(i int) error {
		// get document with filter
		d, err := common.AssertType[*types.Document](must.NotFail(deletes.Get(i)))
		if err != nil {
			return err
		}

		if err := common.Unimplemented(d, "collation", "hint"); err != nil {
			return err
		}

		// get filter from document
		var filter *types.Document
		if filter, err = common.GetOptionalParam(d, "q", filter); err != nil {
			return err
		}

		// TODO https://github.com/FerretDB/FerretDB/issues/982
		var limit int64
		if l, _ := d.Get("limit"); l != nil {
			if limit, err = common.GetWholeNumberParam(l); err != nil {
				return err
			}
		}

		var fp tigrisdb.FetchParam

		if fp.DB, err = common.GetRequiredParam[string](document, "$db"); err != nil {
			return err
		}
		collectionParam, err := document.Get(document.Command())
		if err != nil {
			return err
		}

		var ok bool
		if fp.Collection, ok = collectionParam.(string); !ok {
			return common.NewErrorMsg(
				common.ErrBadValue,
				fmt.Sprintf("collection name has invalid type %s", common.AliasFromType(collectionParam)),
			)
		}

		// fetch current items from collection
		fetchedDocs, err := h.db.QueryDocuments(ctx, fp)
		if err != nil {
			return err
		}

		resDocs := make([]*types.Document, 0, 16)
		// iterate through every row and delete matching ones
		for _, doc := range fetchedDocs {
			// fetch current items from collection
			matches, err := common.FilterDocument(doc, filter)
			if err != nil {
				return err
			}

			if !matches {
				continue
			}

			resDocs = append(resDocs, doc)
		}

		if resDocs, err = common.LimitDocuments(resDocs, limit); err != nil {
			return err
		}

		// if no field is matched in a row, go to the next one
		if len(resDocs) == 0 {
			return nil
		}

		res, err := h.delete(ctx, fp, resDocs)
		if err != nil {
			return err
		}

		deleted += int32(res)

		return nil
	}

	delErrors := new(common.WriteErrors)

	var reply wire.OpMsg

	// process every delete filter
	for i := 0; i < deletes.Len(); i++ {
		err = processQuery(i)
		if err != nil {
			delErrors.Append(err, int32(i))

			// Delete statements in the `deletes` field are not transactional.
			// It means that we run each delete statement separately.
			// If `ordered` is set as `true`, we don't execute the remaining statements
			// after the first failure.
			// If `ordered` is set as `false`,  we execute all the statements and return
			// the list of errors corresponding to the failed statements.
			if ordered {
				break
			}
		}
	}

	var replyDoc *types.Document

	// if there are delete errors append writeErrors field
	if len(*delErrors) > 0 {
		replyDoc = delErrors.Document()
	} else {
		replyDoc = must.NotFail(types.NewDocument(
			"ok", float64(1),
		))
	}

	must.NoError(replyDoc.Set("n", deleted))

	err = reply.SetSections(wire.OpMsgSection{
		Documents: []*types.Document{replyDoc},
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &reply, nil
}

// delete deletes documents by _id.
func (h *Handler) delete(ctx context.Context, fp tigrisdb.FetchParam, docs []*types.Document) (int, error) {
	ids := make([]map[string]any, len(docs))
	for i, doc := range docs {
		id := must.NotFail(tjson.Marshal(must.NotFail(doc.Get("_id"))))
		ids[i] = map[string]any{"_id": map[string]json.RawMessage{"$eq": id}}
	}

	var f driver.Filter
	switch len(ids) {
	case 0:
		f = driver.Filter(`{}`)
	case 1:
		f = must.NotFail(json.Marshal(ids[0]))
	default:
		f = must.NotFail(json.Marshal(map[string]any{"$or": ids}))
	}

	h.L.Sugar().Debugf("Delete filter: %s", f)

	_, err := h.db.Driver.UseDatabase(fp.DB).Delete(ctx, fp.Collection, f)
	if err != nil {
		return 0, lazyerrors.Error(err)
	}

	return len(ids), nil
}
