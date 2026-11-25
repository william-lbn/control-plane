package realtime

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
)

// 用真实 PostgreSQL wire 编解码和内存连接覆盖异步消息，不用生产连接，
// 不模拟持久化成功。错误/空帧/提前结束不得触发任何 invalidation。
func TestConsumeAsynchronousProtocolMessages(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		message  pgproto3.BackendMessage
		accepted bool
	}{
		{"notice", &pgproto3.NoticeResponse{Severity: "NOTICE", Message: "fixture"}, true},
		{"parameter_status", &pgproto3.ParameterStatus{Name: "TimeZone", Value: "UTC"}, true},
		{"server_error", &pgproto3.ErrorResponse{Severity: "ERROR", Code: "42501", Message: "fixture-private-value"}, false},
		{"ended_stream", &pgproto3.ReadyForQuery{TxStatus: 'I'}, false},
		{"empty_copy_data", &pgproto3.CopyData{}, false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			client, server := net.Pipe()
			defer client.Close()
			defer server.Close()
			done := make(chan error, 1)
			go func() { done <- logicalProtocolFixture(server, scenario.message, scenario.accepted) }()
			config, err := pgconn.ParseConfig("postgres://fixture@127.0.0.1/realtime_wire_test?sslmode=disable")
			if err != nil {
				t.Fatal(err)
			}
			config.DialFunc = func(context.Context, string, string) (net.Conn, error) { return client, nil }
			conn, err := pgconn.ConnectConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			source := Source{SystemID: "42", Timeline: 1, Database: "realtime_wire_test", Slot: "fixture_slot",
				Publication: "fixture_publication", ConfirmedLSN: 10, Relations: []Relation{{"public", "sample"}},
				MaxFrameBytes: 128 * 1024, FeedbackInterval: time.Hour}
			persistenceFailure := errors.New("fixture durable sink rejected commit")
			var commits []Commit
			err = Consume(ctx, conn, source, func(_ context.Context, commit Commit) error {
				commits = append(commits, commit)
				return persistenceFailure
			})
			if scenario.accepted {
				if !errors.Is(err, persistenceFailure) || len(commits) != 1 || commits[0].LSN != 100 ||
					commits[0].EndLSN != 101 || len(commits[0].Relations) != 1 || commits[0].Relations[0] != source.Relations[0] {
					t.Fatalf("legal asynchronous message interrupted the original committed stream: %v", err)
				}
			} else if err == nil || errors.Is(err, persistenceFailure) || len(commits) != 0 {
				t.Fatal("failed or ended stream produced an invalidation")
			}
			if scenario.name == "server_error" {
				var sourceError *pgconn.PgError
				if !errors.As(err, &sourceError) || sourceError.Code != "42501" || strings.Contains(err.Error(), "fixture-private-value") {
					t.Fatal("stream did not preserve safe SQLSTATE classification")
				}
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		})
	}
}

func logicalProtocolFixture(conn net.Conn, injected pgproto3.BackendMessage, sendCommit bool) error {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	backend := pgproto3.NewBackend(conn, conn)
	if _, err := backend.ReceiveStartupMessage(); err != nil {
		return err
	}
	backend.Send(&pgproto3.AuthenticationOk{})
	backend.Send(&pgproto3.ParameterStatus{Name: "server_version", Value: "17.0"})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return err
	}
	msg, err := backend.Receive()
	if query, ok := msg.(*pgproto3.Query); err != nil || !ok || strings.TrimSpace(query.String) != "IDENTIFY_SYSTEM" {
		return fmt.Errorf("fixture expected original identity query, got %T: %v", msg, err)
	}
	fields := make([]pgproto3.FieldDescription, 4)
	for index, name := range []string{"systemid", "timeline", "xlogpos", "dbname"} {
		fields[index] = pgproto3.FieldDescription{Name: []byte(name), DataTypeOID: 25, DataTypeSize: -1}
	}
	backend.Send(&pgproto3.RowDescription{Fields: fields})
	backend.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("42"), []byte("1"), []byte("0/A"), []byte("realtime_wire_test")}})
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("IDENTIFY_SYSTEM")})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := backend.Flush(); err != nil {
		return err
	}
	msg, err = backend.Receive()
	if query, ok := msg.(*pgproto3.Query); err != nil || !ok || !strings.HasPrefix(query.String, "START_REPLICATION SLOT fixture_slot LOGICAL") {
		return fmt.Errorf("fixture expected exact logical start, got %T: %v", msg, err)
	}
	backend.Send(&pgproto3.CopyBothResponse{})
	backend.Send(injected)
	if sendCommit {
		begin := binary.BigEndian.AppendUint64([]byte{'B'}, 100)
		begin = binary.BigEndian.AppendUint64(begin, 0)
		begin = binary.BigEndian.AppendUint32(begin, 1)
		relation := binary.BigEndian.AppendUint32([]byte{'R'}, 1)
		relation = append(relation, []byte("public\x00sample\x00d\x00\x00")...)
		insert := binary.BigEndian.AppendUint32([]byte{'I'}, 1)
		insert = append(insert, 'N', 0, 0)
		commit := binary.BigEndian.AppendUint64([]byte{'C', 0}, 100)
		commit = binary.BigEndian.AppendUint64(commit, 101)
		commit = binary.BigEndian.AppendUint64(commit, 0)
		for _, frame := range [][]byte{begin, relation, insert, commit} {
			wal := binary.BigEndian.AppendUint64([]byte{'w'}, 10)
			wal = binary.BigEndian.AppendUint64(wal, 101)
			wal = binary.BigEndian.AppendUint64(wal, 0)
			backend.Send(&pgproto3.CopyData{Data: append(wal, frame...)})
		}
	}
	if err := backend.Flush(); err != nil {
		return err
	}
	msg, err = backend.Receive()
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil // Consume 拥有连接；流退出后允许直接关闭。
	}
	if _, terminated := msg.(*pgproto3.Terminate); terminated && err == nil {
		return nil
	}
	if _, ok := msg.(*pgproto3.CopyDone); err != nil || !ok {
		return fmt.Errorf("fixture expected bounded stream cleanup, got %T: %v", msg, err)
	}
	backend.Send(&pgproto3.CopyDone{})
	backend.Send(&pgproto3.CommandComplete{CommandTag: []byte("COPY 0")})
	backend.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	err = backend.Flush()
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return nil
	}
	return err
}
