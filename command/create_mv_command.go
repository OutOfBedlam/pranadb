package command

import (
	"fmt"
	"github.com/squareup/pranadb/cluster"
	"github.com/squareup/pranadb/command/parser"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/errors"
	"github.com/squareup/pranadb/meta"
	"github.com/squareup/pranadb/parplan"
	"github.com/squareup/pranadb/push"
	"sync"
)

type CreateMVCommand struct {
	lock           sync.Mutex
	e              *Executor
	pl             *parplan.Planner
	schema         *common.Schema
	createMVSQL    string
	tableSequences []uint64
	mv             *push.MaterializedView
	ast            *parser.CreateMaterializedView
	toDeleteBatch  *cluster.ToDeleteBatch
}

func (c *CreateMVCommand) CommandType() DDLCommandType {
	return DDLCommandTypeCreateMV
}

func (c *CreateMVCommand) SchemaName() string {
	return c.schema.Name
}

func (c *CreateMVCommand) SQL() string {
	return c.createMVSQL
}

func (c *CreateMVCommand) TableSequences() []uint64 {
	return c.tableSequences
}

func (c *CreateMVCommand) LockName() string {
	return c.schema.Name + "/"
}

func NewOriginatingCreateMVCommand(e *Executor, pl *parplan.Planner, schema *common.Schema, sql string, tableSequences []uint64, ast *parser.CreateMaterializedView) *CreateMVCommand {
	pl.RefreshInfoSchema()
	return &CreateMVCommand{
		e:              e,
		schema:         schema,
		pl:             pl,
		ast:            ast,
		createMVSQL:    sql,
		tableSequences: tableSequences,
	}
}

func NewCreateMVCommand(e *Executor, schemaName string, createMVSQL string, tableSequences []uint64) *CreateMVCommand {
	schema := e.metaController.GetOrCreateSchema(schemaName)
	pl := parplan.NewPlanner(schema)
	return &CreateMVCommand{
		e:              e,
		schema:         schema,
		pl:             pl,
		createMVSQL:    createMVSQL,
		tableSequences: tableSequences,
	}
}

func (c *CreateMVCommand) OnPhase(phase int32) error {
	switch phase {
	case 0:
		return c.onPhase0()
	case 1:
		return c.onPhase1()
	case 2:
		return c.onPhase2()
	default:
		panic("invalid phase")
	}
}

func (c *CreateMVCommand) NumPhases() int {
	return 3
}

func (c *CreateMVCommand) Before() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Mainly validation

	mv, err := c.createMVFromAST(c.ast)
	if err != nil {
		return errors.WithStack(err)
	}
	c.mv = mv
	_, ok := c.e.metaController.GetMaterializedView(mv.Info.SchemaName, mv.Info.Name)
	if ok {
		return errors.NewMaterializedViewAlreadyExistsError(mv.Info.SchemaName, mv.Info.Name)
	}
	rows, err := c.e.pullEngine.ExecuteQuery("sys",
		fmt.Sprintf("select id from tables where schema_name='%s' and name='%s' and kind='%s'", c.mv.Info.SchemaName, c.mv.Info.Name, meta.TableKindMaterializedView))
	if err != nil {
		return errors.WithStack(err)
	}
	if rows.RowCount() != 0 {
		return errors.Errorf("source with name %s.%s already exists in storage", c.mv.Info.SchemaName, c.mv.Info.Name)
	}

	return err
}

func (c *CreateMVCommand) onPhase0() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// If receiving on prepare from broadcast on the originating node, mv will already be set
	// this means we do not have to parse the ast twice!
	if c.mv == nil {
		mv, err := c.createMV()
		if err != nil {
			return errors.WithStack(err)
		}
		c.mv = mv
	}

	// We store rows in the to_delete table - if MV creation fails (e.g. node crashes) then on restart the MV state will
	// be cleaned up - we have to add a prefix for each shard as the shard id comes first in the key
	var err error
	c.toDeleteBatch, err = storeToDeleteBatch(c.mv.Info.ID, c.e.cluster)
	if err != nil {
		return err
	}

	// We must first connect any aggregations in the MV as remote consumers as they might have rows forwarded to them
	// during the MV fill process. This must be done on all nodes before we start the fill
	// We do not join the MV up to it's feeding sources or MVs at this point
	return c.mv.Connect(false, true)
}

func (c *CreateMVCommand) onPhase1() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Fill the MV from it's feeding sources and MVs
	return c.mv.Fill()
}

func (c *CreateMVCommand) onPhase2() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// The fill can cause rows to be forwarded - to make sure they're all processed we must wait for all schedulers
	// on all nodes - this must be done after fill has completed on all nodes
	if err := c.e.pushEngine.WaitForSchedulers(); err != nil {
		return err
	}

	// The MV is now created and filled on all nodes but it isn't currently registered so it can't be used by clients
	// We register it now
	if err := c.e.pushEngine.RegisterMV(c.mv); err != nil {
		return errors.WithStack(err)
	}
	if err := c.e.metaController.RegisterMaterializedView(c.mv.Info, c.mv.InternalTables); err != nil {
		return err
	}
	// Maybe inject an error after fill and after row in tables table is persisted but before to_delete rows removed
	if err := c.e.FailureInjector().GetFailpoint("create_mv_2").CheckFail(); err != nil {
		return err
	}

	// Now delete rows from the to_delete table
	return c.e.cluster.RemoveToDeleteBatch(c.toDeleteBatch)
}

func (c *CreateMVCommand) AfterPhase(phase int32) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if phase == 1 {
		// Maybe inject an error after fill but before row in tables table is persisted
		if err := c.e.FailureInjector().GetFailpoint("create_mv_1").CheckFail(); err != nil {
			return err
		}

		// We add the MV to the tables table once the fill phase is complete
		// We only do this on the originating node
		// We need to do this *before* the MV is available to clients otherwise a node failure and restart could cause
		// the MV to disappear after it's been used
		return c.e.metaController.PersistMaterializedView(c.mv.Info, c.mv.InternalTables)
	}
	return nil
}

func (c *CreateMVCommand) createMVFromAST(ast *parser.CreateMaterializedView) (*push.MaterializedView, error) {
	mvName := ast.Name.String()
	querySQL := ast.Query.String()
	seqGenerator := common.NewPreallocSeqGen(c.tableSequences)
	tableID := seqGenerator.GenerateSequence()
	return push.CreateMaterializedView(c.e.pushEngine, c.pl, c.schema, mvName, querySQL, tableID, seqGenerator)
}

func (c *CreateMVCommand) createMV() (*push.MaterializedView, error) {
	ast, err := parser.Parse(c.createMVSQL)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if ast.Create == nil || ast.Create.MaterializedView == nil {
		return nil, errors.Errorf("not a create materialized view %s", c.createMVSQL)
	}
	return c.createMVFromAST(ast.Create.MaterializedView)
}
