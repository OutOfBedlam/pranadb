package command

import (
	"fmt"
	"sync"

	"github.com/alecthomas/repr"
	"github.com/squareup/pranadb/command/parser"
	"github.com/squareup/pranadb/command/parser/selector"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/errors"
	"github.com/squareup/pranadb/meta"
	"github.com/squareup/pranadb/push/source"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type CreateSourceCommand struct {
	lock           sync.Mutex
	e              *Executor
	schemaName     string
	sql            string
	tableSequences []uint64
	ast            *parser.CreateSource
	sourceInfo     *common.SourceInfo
	source         *source.Source
}

func (c *CreateSourceCommand) CommandType() DDLCommandType {
	return DDLCommandTypeCreateSource
}

func (c *CreateSourceCommand) SchemaName() string {
	return c.schemaName
}

func (c *CreateSourceCommand) SQL() string {
	return c.sql
}

func (c *CreateSourceCommand) TableSequences() []uint64 {
	return c.tableSequences
}

func (c *CreateSourceCommand) LockName() string {
	return c.schemaName + "/"
}

func NewOriginatingCreateSourceCommand(e *Executor, schemaName string, sql string, tableSequences []uint64, ast *parser.CreateSource) *CreateSourceCommand {
	return &CreateSourceCommand{
		e:              e,
		schemaName:     schemaName,
		sql:            sql,
		tableSequences: tableSequences,
		ast:            ast,
	}
}

func NewCreateSourceCommand(e *Executor, schemaName string, sql string, tableSequences []uint64) *CreateSourceCommand {
	return &CreateSourceCommand{
		e:              e,
		schemaName:     schemaName,
		sql:            sql,
		tableSequences: tableSequences,
	}
}

func (c *CreateSourceCommand) Before() error {
	c.lock.Lock()
	defer c.lock.Unlock()
	var err error
	c.sourceInfo, err = c.getSourceInfo(c.ast)
	if err != nil {
		return errors.WithStack(err)
	}
	return c.validate()
}

func (c *CreateSourceCommand) validate() error {
	_, ok := c.e.metaController.GetSource(c.schemaName, c.sourceInfo.Name)
	if ok {
		return errors.NewSourceAlreadyExistsError(c.schemaName, c.sourceInfo.Name)
	}
	rows, err := c.e.pullEngine.ExecuteQuery("sys",
		fmt.Sprintf("select id from tables where schema_name='%s' and name='%s' and kind='%s'", c.sourceInfo.SchemaName, c.sourceInfo.Name, meta.TableKindSource))
	if err != nil {
		return errors.WithStack(err)
	}
	if rows.RowCount() != 0 {
		return errors.Errorf("source with name %s.%s already exists in storage", c.sourceInfo.SchemaName, c.sourceInfo.Name)
	}

	topicInfo := c.sourceInfo.TopicInfo

	for _, enc := range []common.KafkaEncoding{topicInfo.HeaderEncoding, topicInfo.KeyEncoding, topicInfo.ValueEncoding} {
		if enc.Encoding != common.EncodingProtobuf {
			continue
		}
		_, err := c.e.protoRegistry.FindDescriptorByName(protoreflect.FullName(enc.SchemaName))
		if err != nil {
			return errors.NewPranaErrorf(errors.UnknownTopicEncoding, "proto message %q not registered", enc.SchemaName)
		}
	}

	for _, sel := range topicInfo.ColSelectors {
		if sel.MetaKey == nil && len(sel.Selector) == 0 {
			return errors.NewPranaErrorf(errors.InvalidSelector, "invalid column selector %q", sel)
		}
		if sel.MetaKey != nil {
			f := *sel.MetaKey
			if !(f == "header" || f == "key" || f == "timestamp") {
				return errors.NewPranaErrorf(errors.InvalidSelector, `invalid metadata key in column selector %q. Valid values are "header", "key", "timestamp".`, sel)
			}
		}
	}

	return nil
}

func (c *CreateSourceCommand) OnPhase(phase int32) error {
	switch phase {
	case 0:
		return c.onPhase0()
	case 1:
		return c.onPhase1()
	default:
		panic("invalid phase")
	}
}

func (c *CreateSourceCommand) NumPhases() int {
	return 2
}

func (c *CreateSourceCommand) onPhase0() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// If receiving on prepare from broadcast on the originating node, mvInfo will already be set
	// this means we do not have to parse the ast twice!
	if c.sourceInfo == nil {
		ast, err := parser.Parse(c.sql)
		if err != nil {
			return errors.WithStack(err)
		}
		if ast.Create == nil || ast.Create.Source == nil {
			return errors.Errorf("not a create source %s", c.sql)
		}
		c.sourceInfo, err = c.getSourceInfo(ast.Create.Source)
		if err != nil {
			return errors.WithStack(err)
		}
	}

	// Create source in push engine so it can receive forwarded rows, do not activate consumers yet
	src, err := c.e.pushEngine.CreateSource(c.sourceInfo)
	if err != nil {
		return errors.WithStack(err)
	}
	c.source = src
	return err
}

func (c *CreateSourceCommand) onPhase1() error {
	c.lock.Lock()
	defer c.lock.Unlock()

	// Activate the message consumers for the source
	if err := c.source.Start(); err != nil {
		return errors.WithStack(err)
	}

	// Register the source in the in memory meta data
	return c.e.metaController.RegisterSource(c.sourceInfo)
}

func (c *CreateSourceCommand) AfterPhase(phase int32) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	if phase == 0 {
		// We persist the source *before* it is registered - otherwise if failure occurs source can disappear after
		// being used
		return c.e.metaController.PersistSource(c.sourceInfo)
	}
	return nil
}

// nolint: gocyclo
func (c *CreateSourceCommand) getSourceInfo(ast *parser.CreateSource) (*common.SourceInfo, error) {
	var (
		colNames []string
		colTypes []common.ColumnType
		colIndex = map[string]int{}
		pkCols   []int
	)
	for i, option := range ast.Options {
		switch {
		case option.Column != nil:
			// Convert AST column definition to a ColumnType.
			col := option.Column
			colIndex[col.Name] = i
			colNames = append(colNames, col.Name)
			colType, err := col.ToColumnType()
			if err != nil {
				return nil, errors.WithStack(err)
			}
			colTypes = append(colTypes, colType)

		case len(option.PrimaryKey) > 0:
			for _, pk := range option.PrimaryKey {
				index, ok := colIndex[pk]
				if !ok {
					return nil, errors.Errorf("invalid primary key column %q", option.PrimaryKey)
				}
				pkCols = append(pkCols, index)
			}

		default:
			panic(repr.String(option))
		}
	}

	var (
		headerEncoding, keyEncoding, valueEncoding common.KafkaEncoding
		propsMap                                   map[string]string
		colSelectors                               []selector.ColumnSelector
		brokerName, topicName                      string
	)
	for _, opt := range ast.TopicInformation {
		switch {
		case opt.HeaderEncoding != "":
			headerEncoding = common.KafkaEncodingFromString(opt.HeaderEncoding)
			if headerEncoding.Encoding == common.EncodingUnknown {
				return nil, errors.NewPranaErrorf(errors.UnknownTopicEncoding, "Unknown topic encoding %s", opt.HeaderEncoding)
			}
		case opt.KeyEncoding != "":
			keyEncoding = common.KafkaEncodingFromString(opt.KeyEncoding)
			if keyEncoding.Encoding == common.EncodingUnknown {
				return nil, errors.NewPranaErrorf(errors.UnknownTopicEncoding, "Unknown topic encoding %s", opt.KeyEncoding)
			}
		case opt.ValueEncoding != "":
			valueEncoding = common.KafkaEncodingFromString(opt.ValueEncoding)
			if valueEncoding.Encoding == common.EncodingUnknown {
				return nil, errors.NewPranaErrorf(errors.UnknownTopicEncoding, "Unknown topic encoding %s", opt.ValueEncoding)
			}
		case opt.Properties != nil:
			propsMap = make(map[string]string, len(opt.Properties))
			for _, prop := range opt.Properties {
				propsMap[prop.Key] = prop.Value
			}
		case opt.ColSelectors != nil:
			cs := opt.ColSelectors
			colSelectors = make([]selector.ColumnSelector, len(cs))
			for i := 0; i < len(cs); i++ {
				colSelectors[i] = cs[i].ToSelector()
			}
		case opt.BrokerName != "":
			brokerName = opt.BrokerName
		case opt.TopicName != "":
			topicName = opt.TopicName
		}
	}
	if headerEncoding == common.KafkaEncodingUnknown {
		return nil, errors.NewPranaError(errors.InvalidStatement, "headerEncoding is required")
	}
	if keyEncoding == common.KafkaEncodingUnknown {
		return nil, errors.NewPranaError(errors.InvalidStatement, "keyEncoding is required")
	}
	if valueEncoding == common.KafkaEncodingUnknown {
		return nil, errors.NewPranaError(errors.InvalidStatement, "valueEncoding is required")
	}
	if brokerName == "" {
		return nil, errors.NewPranaError(errors.InvalidStatement, "brokerName is required")
	}
	if topicName == "" {
		return nil, errors.NewPranaError(errors.InvalidStatement, "topicName is required")
	}
	lc := len(colSelectors)
	if lc > 0 && lc != len(colTypes) {
		return nil, errors.NewPranaErrorf(errors.WrongNumberColumnSelectors,
			"Number of column selectors (%d) must match number of columns (%d)", lc, len(colTypes))
	}

	topicInfo := &common.TopicInfo{
		BrokerName:     brokerName,
		TopicName:      topicName,
		HeaderEncoding: headerEncoding,
		KeyEncoding:    keyEncoding,
		ValueEncoding:  valueEncoding,
		ColSelectors:   colSelectors,
		Properties:     propsMap,
	}
	tableInfo := common.TableInfo{
		ID:             c.tableSequences[0],
		SchemaName:     c.schemaName,
		Name:           ast.Name,
		PrimaryKeyCols: pkCols,
		ColumnNames:    colNames,
		ColumnTypes:    colTypes,
		IndexInfos:     nil,
	}
	return &common.SourceInfo{
		TableInfo: &tableInfo,
		TopicInfo: topicInfo,
	}, nil
}
