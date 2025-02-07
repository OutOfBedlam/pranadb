// Package api contains the over-the-wire gRPC server for PranaDB.
package api

import (
	"context"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/squareup/pranadb/meta"

	log "github.com/sirupsen/logrus"
	"github.com/squareup/pranadb/command"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/conf"
	"github.com/squareup/pranadb/errors"
	"github.com/squareup/pranadb/protolib"
	"github.com/squareup/pranadb/protos/squareup/cash/pranadb/v1/service"
	"google.golang.org/grpc"
	_ "google.golang.org/grpc/encoding/gzip" // Registers gzip (de)-compressor
	"google.golang.org/grpc/reflection"
	"google.golang.org/protobuf/types/known/emptypb"
)

var _ service.PranaDBServiceServer = &Server{}

type Server struct {
	lock           sync.Mutex
	started        bool
	ce             *command.Executor
	serverAddress  string
	gsrv           *grpc.Server
	errorSequence  int64
	protoRegistry  *protolib.ProtoRegistry
	metaController *meta.Controller
}

func NewAPIServer(metaController *meta.Controller, ce *command.Executor, protobufs *protolib.ProtoRegistry, cfg conf.Config) *Server {
	return &Server{
		metaController: metaController,
		ce:             ce,
		protoRegistry:  protobufs,
		serverAddress:  cfg.APIServerListenAddresses[cfg.NodeID],
	}
}

func (s *Server) Start() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.started {
		return nil
	}
	list, err := net.Listen("tcp", s.serverAddress)
	if err != nil {
		return errors.WithStack(err)
	}
	s.gsrv = grpc.NewServer()
	reflection.Register(s.gsrv)
	service.RegisterPranaDBServiceServer(s.gsrv, s)
	s.started = true
	go s.startServer(list)
	return nil
}

func (s *Server) startServer(list net.Listener) {
	err := s.gsrv.Serve(list) //nolint:ifshort
	s.lock.Lock()
	defer s.lock.Unlock()
	s.started = false
	if err != nil {
		log.Errorf("grpc server listen failed: %v", err)
	}
}

func (s *Server) Stop() error {
	s.lock.Lock()
	defer s.lock.Unlock()
	if !s.started {
		return nil
	}
	s.gsrv.Stop()
	s.errorSequence = 0
	return nil
}

func (s *Server) ExecuteSQLStatement(in *service.ExecuteSQLStatementRequest,
	stream service.PranaDBService_ExecuteSQLStatementServer) error {
	defer common.PanicHandler()
	var schema *common.Schema
	if in.Schema != "" {
		schema = s.metaController.GetOrCreateSchema(in.Schema)
	}
	execCtx := s.ce.CreateExecutionContext(schema)
	defer func() {
		s.metaController.DeleteSchemaIfEmpty(schema)
	}()

	executor, err := s.ce.ExecuteSQLStatement(execCtx, in.Statement)
	if err != nil {
		log.Errorf("failed to execute statement %+v", err)
		var perr errors.PranaError
		if errors.As(err, &perr) {
			return perr
		}
		// For internal errors we don't return internal error messages to the CLI as this would leak
		// server implementation details. Instead, we generate a sequence number and add that to the message
		// and log the internal error in the server logs with the sequence number so it can be looked up
		seq := atomic.AddInt64(&s.errorSequence, 1)
		perr = errors.NewInternalError(seq)
		log.Errorf("internal error occurred with sequence number %d\n%v", seq, err)
		return perr
	}

	// First send column definitions.
	columns := &service.Columns{}
	names := executor.ColNames()
	for i, typ := range executor.ColTypes() {
		name := names[i]
		column := &service.Column{
			Name: name,
			Type: service.ColumnType(typ.Type),
		}
		if typ.Type == common.TypeDecimal {
			column.DecimalParams = &service.DecimalParams{
				DecimalPrecision: uint32(typ.DecPrecision),
				DecimalScale:     uint32(typ.DecScale),
			}
		}
		columns.Columns = append(columns.Columns, column)
	}
	if err := stream.Send(&service.ExecuteSQLStatementResponse{Result: &service.ExecuteSQLStatementResponse_Columns{Columns: columns}}); err != nil {
		return errors.WithStack(err)
	}

	// Then start sending pages until complete.
	numCols := len(executor.ColTypes())
	for {
		// Transcode rows.
		rows, err := executor.GetRows(int(in.PageSize))
		if err != nil {
			return errors.WithStack(err)
		}
		prows := make([]*service.Row, rows.RowCount())
		for i := 0; i < rows.RowCount(); i++ {
			row := rows.GetRow(i)
			colVals := make([]*service.ColValue, numCols)
			for colNum, colType := range executor.ColTypes() {
				colVal := &service.ColValue{}
				colVals[colNum] = colVal
				if row.IsNull(colNum) {
					colVal.Value = &service.ColValue_IsNull{IsNull: true}
				} else {
					switch colType.Type {
					case common.TypeTinyInt, common.TypeInt, common.TypeBigInt:
						colVal.Value = &service.ColValue_IntValue{IntValue: row.GetInt64(colNum)}
					case common.TypeDouble:
						colVal.Value = &service.ColValue_FloatValue{FloatValue: row.GetFloat64(colNum)}
					case common.TypeVarchar:
						colVal.Value = &service.ColValue_StringValue{StringValue: row.GetString(colNum)}
					case common.TypeDecimal:
						dec := row.GetDecimal(colNum)
						// We encode the decimal as a string
						colVal.Value = &service.ColValue_StringValue{StringValue: dec.String()}
					case common.TypeTimestamp:
						ts := row.GetTimestamp(colNum)
						gt, err := ts.GoTime(time.UTC)
						if err != nil {
							return err
						}
						// We encode a datetime as *microseconds* past epoch
						unixTime := gt.UnixNano() / 1000
						colVal.Value = &service.ColValue_IntValue{IntValue: unixTime}
					default:
						panic(fmt.Sprintf("unexpected column type %d", colType.Type))
					}
				}
			}
			pRow := &service.Row{Values: colVals}
			prows[i] = pRow
		}
		numRows := rows.RowCount()
		results := &service.Page{
			Count: uint64(numRows),
			Rows:  prows,
		}
		if err = stream.Send(&service.ExecuteSQLStatementResponse{Result: &service.ExecuteSQLStatementResponse_Page{Page: results}}); err != nil {
			return errors.WithStack(err)
		}
		if numRows < int(in.PageSize) {
			break
		}
	}
	return nil
}

func (s *Server) RegisterProtobufs(ctx context.Context, request *service.RegisterProtobufsRequest) (*emptypb.Empty, error) {
	return &emptypb.Empty{}, s.protoRegistry.RegisterFiles(request.GetDescriptors())
}

func (s *Server) GetListenAddress() string {
	return s.serverAddress
}
