package pull

import (
	"fmt"
	"github.com/squareup/pranadb/sharder"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/squareup/pranadb/cluster"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/errors"
	"github.com/squareup/pranadb/execctx"
	"github.com/squareup/pranadb/meta"
	"github.com/squareup/pranadb/pull/exec"
)

type Engine struct {
	lock              sync.RWMutex
	started           bool
	queryExecCtxCache atomic.Value
	cluster           cluster.Cluster
	metaController    *meta.Controller
	nodeID            int
	shrder            *sharder.Sharder
	available         common.AtomicBool
}

func NewPullEngine(cluster cluster.Cluster, metaController *meta.Controller, shrder *sharder.Sharder) *Engine {
	engine := Engine{
		cluster:        cluster,
		metaController: metaController,
		nodeID:         cluster.GetNodeID(),
		shrder:         shrder,
	}
	engine.queryExecCtxCache.Store(new(sync.Map))
	return &engine
}

func (p *Engine) Start() error {
	p.lock.Lock()
	defer p.lock.Unlock()
	if p.started {
		return nil
	}
	p.started = true
	return nil
}

func (p *Engine) Stop() error {
	p.lock.Lock()
	defer p.lock.Unlock()
	if !p.started {
		return nil
	}
	p.queryExecCtxCache.Store(new(sync.Map)) // Clear the internal state
	p.available.Set(false)
	p.started = false
	return nil
}

func (p *Engine) SetAvailable() {
	p.available.Set(true)
}

func (p *Engine) BuildPullQuery(execCtx *execctx.ExecutionContext, query string) (exec.PullExecutor, error) {
	qi := execCtx.QueryInfo
	qi.ExecutionID = execCtx.ID
	qi.SchemaName = execCtx.Schema.Name
	qi.Query = query
	ast, err := execCtx.Planner().Parse(query)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	logicalPlan, err := execCtx.Planner().BuildLogicalPlan(ast, false)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	physicalPlan, err := execCtx.Planner().BuildPhysicalPlan(logicalPlan, true)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	return p.buildPullDAGWithOutputNames(execCtx, logicalPlan, physicalPlan, false)
}

// ExecuteRemotePullQuery - executes a pull query received from another node
//nolint:gocyclo
func (p *Engine) ExecuteRemotePullQuery(queryInfo *cluster.QueryExecutionInfo) (*common.Rows, error) {
	// We need to prevent queries being executed before the schemas have been loaded, however queries from the
	// system schema don't need schema to be loaded as that schema is hardcoded in the meta controller
	// In order to actually load other schemas we need to execute queries from the system query so we need a way
	// of executing system queries during the startup process
	if !queryInfo.SystemQuery && !p.available.Get() {
		return nil, errors.New("pull engine not available")
	}
	if queryInfo.ExecutionID == "" {
		panic("empty execution id")
	}
	s, ok := p.getCachedExecCtx(queryInfo.ExecutionID)
	newExecution := false
	if !ok {
		schema := p.metaController.GetOrCreateSchema(queryInfo.SchemaName)
		s = execctx.NewExecutionContext(queryInfo.ExecutionID, schema)
		newExecution = true
		s.QueryInfo = queryInfo
		ast, err := s.Planner().Parse(queryInfo.Query)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		logic, err := s.Planner().BuildLogicalPlan(ast, true)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		physicalPlan, err := s.Planner().BuildPhysicalPlan(logic, true)
		if err != nil {
			return nil, err
		}
		dag, err := p.buildPullDAG(s, physicalPlan, false)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		remExecutor := p.findRemoteExecutor(dag)
		if remExecutor == nil {
			return nil, errors.Error("cannot find remote executor")
		}
		s.CurrentQuery = remExecutor.RemoteDag
	} else if s.QueryInfo.Query != queryInfo.Query {
		// Sanity check
		panic(fmt.Sprintf("Already executing query is %s but passed in query is %s", s.QueryInfo.Query, queryInfo.Query))
	}
	rows, err := p.getRowsFromCurrentQuery(s, int(queryInfo.Limit))
	if err != nil {
		// Make sure we remove current query in case of error
		s.CurrentQuery = nil
		return nil, errors.WithStack(err)
	}
	if newExecution && s.CurrentQuery != nil {
		// We only need to store the ctx for later if there are more rows to return
		p.execCtxCache().Store(queryInfo.ExecutionID, s)
	} else if s.CurrentQuery == nil {
		// We can delete the exec ctx if current query is complete
		p.execCtxCache().Delete(queryInfo.ExecutionID)
	}
	return rows, errors.WithStack(err)
}

func (p *Engine) getRowsFromCurrentQuery(execCtx *execctx.ExecutionContext, limit int) (*common.Rows, error) {
	rows, err := CurrentQuery(execCtx).GetRows(limit)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	if rows.RowCount() < limit {
		// Query is complete - we can remove it
		execCtx.CurrentQuery = nil
	}
	return rows, nil
}

func (p *Engine) getCachedExecCtx(ctxID string) (*execctx.ExecutionContext, bool) {
	d, ok := p.execCtxCache().Load(ctxID)
	if !ok {
		return nil, false
	}
	s, ok := d.(*execctx.ExecutionContext)
	if !ok {
		panic("invalid type in remote queries")
	}
	return s, true
}

func (p *Engine) findRemoteExecutor(executor exec.PullExecutor) *exec.RemoteExecutor {
	// We only execute the part of the dag beyond the table reader - this is the remote part
	remExecutor, ok := executor.(*exec.RemoteExecutor)
	if ok {
		return remExecutor
	}
	for _, child := range executor.GetChildren() {
		remExecutor := p.findRemoteExecutor(child)
		if remExecutor != nil {
			return remExecutor
		}
	}
	return nil
}

func (p *Engine) NumCachedExecCtxs() (int, error) {
	numEntries := 0
	p.execCtxCache().Range(func(key, value interface{}) bool {
		numEntries++
		return false
	})
	return numEntries, nil
}

func CurrentQuery(ctx *execctx.ExecutionContext) exec.PullExecutor {
	v := ctx.CurrentQuery
	if v == nil {
		return nil
	}
	cq, ok := v.(exec.PullExecutor)
	if !ok {
		panic("invalid current query type")
	}
	return cq
}

func (p *Engine) NodeJoined(nodeID int) {
}

func (p *Engine) NodeLeft(nodeID int) {
	p.clearExecCtxsForNode(nodeID)
}

func (p *Engine) clearExecCtxsForNode(nodeID int) {
	// The node may have crashed - we remove any exec ctxs for that node
	p.lock.Lock()
	defer p.lock.Unlock()

	var idsToRemove []string
	sNodeID := fmt.Sprintf("%d", nodeID)
	p.execCtxCache().Range(func(key, value interface{}) bool {
		ctxID := key.(string) //nolint: forcetypeassert
		i := strings.Index(ctxID, "-")
		if i == -1 {
			panic(fmt.Sprintf("invalid ctx id %s", ctxID))
		}
		snid := ctxID[:i]
		if snid == sNodeID {
			idsToRemove = append(idsToRemove, ctxID)
		}
		return true
	})
	for _, ctxID := range idsToRemove {
		p.execCtxCache().Delete(ctxID)
	}
}

// ExecuteQuery - Lightweight query interface - used internally for loading a moderate amount of rows
func (p *Engine) ExecuteQuery(schemaName string, query string) (rows *common.Rows, err error) {
	schema, ok := p.metaController.GetSchema(schemaName)
	if !ok {
		return nil, errors.Errorf("no such schema %s", schemaName)
	}
	execCtx := execctx.NewExecutionContext("", schema)
	executor, err := p.BuildPullQuery(execCtx, query)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	limit := 1000
	for {
		r, err := executor.GetRows(limit)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if rows == nil {
			rows = r
		} else {
			rows.AppendAll(r)
		}
		if r.RowCount() < limit {
			break
		}
	}
	// No need to close execCtx as no prepared statements
	return rows, nil
}

func (p *Engine) execCtxCache() *sync.Map {
	return p.queryExecCtxCache.Load().(*sync.Map)
}
