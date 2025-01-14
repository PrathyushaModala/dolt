// Copyright 2020 Dolthub, Inc.
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

package dtables

import (
	"context"
	"errors"
	"io"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"

	"github.com/dolthub/dolt/go/libraries/doltcore/diff"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/schema"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/sqlutil"
)

var _ sql.Table = (*SchemaConflictsTable)(nil)

// SchemaConflictsTable is a sql.Table implementation that implements a system table which shows the current conflicts
type SchemaConflictsTable struct {
	dbName string
	ddb    *doltdb.DoltDB
}

// NewSchemaConflictsTable creates a SchemaConflictsTable
func NewSchemaConflictsTable(_ *sql.Context, dbName string, ddb *doltdb.DoltDB) sql.Table {
	return &SchemaConflictsTable{dbName: dbName, ddb: ddb}
}

// Name is a sql.Table interface function which returns the name of the table which is defined by the constant
// SchemaConflictsTableName
func (dt *SchemaConflictsTable) Name() string {
	return doltdb.SchemaConflictsTableName
}

// String is a sql.Table interface function which returns the name of the table which is defined by the constant
// SchemaConflictsTableName
func (dt *SchemaConflictsTable) String() string {
	return doltdb.SchemaConflictsTableName
}

// Schema is a sql.Table interface function that gets the sql.Schema of the log system table.
func (dt *SchemaConflictsTable) Schema() sql.Schema {
	return []*sql.Column{
		{Name: "table_name", Type: types.Text, Source: doltdb.SchemaConflictsTableName, PrimaryKey: true},
		{Name: "base_schema", Type: types.Text, Source: doltdb.SchemaConflictsTableName, PrimaryKey: false},
		{Name: "our_schema", Type: types.Text, Source: doltdb.SchemaConflictsTableName, PrimaryKey: false},
		{Name: "their_schema", Type: types.Text, Source: doltdb.SchemaConflictsTableName, PrimaryKey: false},
		{Name: "description", Type: types.Text, Source: doltdb.SchemaConflictsTableName, PrimaryKey: false},
	}
}

// Collation implements the sql.Table interface.
func (dt *SchemaConflictsTable) Collation() sql.CollationID {
	return sql.Collation_Default
}

// Partitions is a sql.Table interface function that returns a partition of the data.  Conflict data for all tables exists in a single partition.
func (dt *SchemaConflictsTable) Partitions(ctx *sql.Context) (sql.PartitionIter, error) {
	sess := dsess.DSessFromSess(ctx.Session)
	ws, err := sess.WorkingSet(ctx, dt.dbName)
	if err != nil {
		return nil, err
	}
	dbd, _ := sess.GetDbData(ctx, dt.dbName)

	if ws.MergeState() == nil || !ws.MergeState().HasSchemaConflicts() {
		return sql.PartitionsToPartitionIter(), nil
	}

	head, err := sess.GetHeadCommit(ctx, dt.dbName)
	if err != nil {
		return nil, err
	}

	return sql.PartitionsToPartitionIter(schemaConflictsPartition{
		state: ws.MergeState(),
		head:  head,
		ddb:   dbd.Ddb,
	}), nil
}

// PartitionRows is a sql.Table interface function that gets a row iterator for a partition
func (dt *SchemaConflictsTable) PartitionRows(ctx *sql.Context, part sql.Partition) (sql.RowIter, error) {
	p, ok := part.(schemaConflictsPartition)
	if !ok {
		return nil, errors.New("unexpected partition for schema conflicts table")
	}

	base, err := doltdb.GetCommitAncestor(ctx, p.head, p.state.Commit())
	if err != nil {
		return nil, err
	}

	baseRoot, err := base.GetRootValue(ctx)
	if err != nil {
		return nil, err
	}

	var conflicts []schemaConflict
	err = p.state.IterSchemaConflicts(ctx, p.ddb, func(table string, cnf doltdb.SchemaConflict) error {
		c, err := newSchemaConflict(ctx, table, baseRoot, cnf)
		if err != nil {
			return err
		}
		conflicts = append(conflicts, c)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &schemaConflictsIter{
		conflicts: conflicts,
	}, nil
}

type schemaConflictsPartition struct {
	state *doltdb.MergeState
	head  *doltdb.Commit
	ddb   *doltdb.DoltDB
}

func (p schemaConflictsPartition) Key() []byte {
	return []byte(doltdb.SchemaConflictsTableName)
}

type schemaConflict struct {
	table       string
	baseSch     string
	ourSch      string
	theirSch    string
	description string
}

func newSchemaConflict(ctx context.Context, table string, br *doltdb.RootValue, c doltdb.SchemaConflict) (schemaConflict, error) {
	base, err := getCreateTableStatementFromRoot(ctx, table, br)
	if err != nil {
		return schemaConflict{}, err
	}

	ours, err := getCreateTableStatement(table, c.ToSch, c.ToFks, c.ToParentSchemas)
	if err != nil {
		return schemaConflict{}, err
	}

	theirs, err := getCreateTableStatement(table, c.FromSch, c.FromFks, c.FromParentSchemas)
	if err != nil {
		return schemaConflict{}, err
	}

	return schemaConflict{
		table:    table,
		baseSch:  base,
		ourSch:   ours,
		theirSch: theirs,
	}, nil
}

func getCreateTableStatementFromRoot(ctx context.Context, table string, root *doltdb.RootValue) (string, error) {
	schemas, err := root.GetAllSchemas(ctx)
	if err != nil {
		return "", err
	}
	sch := schemas[table]

	fkc, err := root.GetForeignKeyCollection(ctx)
	if err != nil {
		return "", err
	}
	fks, _ := fkc.KeysForTable(table)

	return getCreateTableStatement(table, sch, fks, schemas)
}

func getCreateTableStatement(table string, sch schema.Schema, fks []doltdb.ForeignKey, parents map[string]schema.Schema) (string, error) {
	pkSch, err := sqlutil.FromDoltSchema(table, sch)
	if err != nil {
		return "", err
	}
	return diff.GenerateCreateTableStatement(table, sch, pkSch, fks, parents)
}

type schemaConflictsIter struct {
	conflicts   []schemaConflict
	baseSchemas map[string]schema.Schema
	baseCommit  *doltdb.Commit
}

func (it *schemaConflictsIter) Next(ctx *sql.Context) (sql.Row, error) {
	if len(it.conflicts) == 0 {
		return nil, io.EOF
	}
	c := it.conflicts[0] // pop next conflict
	it.conflicts = it.conflicts[1:]
	return sql.NewRow(c.table, c.baseSch, c.ourSch, c.theirSch, c.description), nil
}

func (it *schemaConflictsIter) Close(ctx *sql.Context) error {
	it.conflicts = nil
	return nil
}
