package core

import (
	"context"
	"fmt"
	"github.com/jackc/pgx"
	"github.com/jackc/pgx/pgtype"
	"log"
	"time"
)

type ReplicationSyncer struct {
	_debug     bool
	_conn      *pgx.ReplicationConn
	_flushLsn  uint64
	_flushMsg  ReplicationMessage
	option     ReplicationOption
	dmlHandler ReplicationDMLHandler
	set        *RelationSet
}

func NewReplicationSyncer(option ReplicationOption, dmlHandler ReplicationDMLHandler) *ReplicationSyncer {
	i := new(ReplicationSyncer)
	i.option = option
	i.dmlHandler = dmlHandler
	i.set = NewRelationSet()
	return i
}

func (t *ReplicationSyncer) Debug() *ReplicationSyncer {
	t._debug = true
	return t
}

func (t *ReplicationSyncer) log(any ...interface{}) {
	if t._debug {
		log.Println(any...)
	}
}

func (t *ReplicationSyncer) conn() (*pgx.ReplicationConn, error) {
	if t._conn == nil {
		conn, err := pgx.ReplicationConnect(t.option.ConnConfig)
		if err != nil {
			return nil, err
		}
		t._conn = conn
	}
	return t._conn, nil
}

func (t *ReplicationSyncer) dump(eventType EventType, relation uint32, row, oldRow []Tuple) (msg ReplicationMessage, err error) {
	msg.SchemaName, msg.TableName = t.set.Assist(relation)
	values, err := t.set.Values(relation, row)
	if err != nil {
		err = fmt.Errorf("error parsing values: %s", err)
		return
	}
	if oldRow != nil {
		if oldValues, er := t.set.Values(relation, oldRow); er == nil {
			msg.Columns = t.dumpColumns(values, oldValues)
		}
	}

	body := make(map[string]interface{}, 0)
	for name, value := range values {
		val := value.Get()
		body[name] = val
	}
	msg.EventType = eventType
	msg.Body = body
	return
}

func (t *ReplicationSyncer) dumpColumns(values, oldValues map[string]pgtype.Value) (res []string) {
	if oldValues == nil || values == nil {
		return nil
	}
	for k, v := range oldValues {
		if newV, ok := values[k]; !ok || newV.Get() != v.Get() {
			res = append(res, k)
		}
	}
	return
}

func (t *ReplicationSyncer) handle(message *pgx.WalMessage) error {
	msg, err := Parse(message.WalData)
	if err != nil {
		return fmt.Errorf("invalid pgoutput message: %s", err)
	}
	switch v := msg.(type) {
	case Relation:
		t.set.Add(v)
	case Insert:
		t._flushMsg, err = t.dump(EventType_INSERT, v.RelationID, v.Row, nil)
		if err != nil {
			return err
		}
	case Update:
		t._flushMsg, err = t.dump(EventType_UPDATE, v.RelationID, v.Row, v.OldRow)
		if err != nil {
			return err
		}
	case Delete:
		t._flushMsg, err = t.dump(EventType_DELETE, v.RelationID, v.Row, nil)
		if err != nil {
			return err
		}
	case Commit:
		if t._flushMsg.SchemaName != "" {
			status := t.dmlHandler(t._flushMsg)
			if status == DMLHandlerStatusSuccess {
				t._flushLsn = message.WalStart
				if err = t.sendStatus(); err != nil {
					return err
				}
			} else if status == DMLHandlerStatusError {
				t.log("dmlHandler:", status)
			}
		}
	}
	return nil
}

func (t *ReplicationSyncer) Start(ctx context.Context) (err error) {
	if err = t.option.valid(); err != nil {
		return
	}
	conn, err := t.conn()
	if err != nil {
		return err
	}
	defer conn.Close()

	//var lsn uint64
	//if startLSN, _ := t.option.Adapter.Get(t.option.SlotName); startLSN > 0 {
	//	t.log("startLSN:", startLSN, pgx.FormatLSN(startLSN))
	//	lsn = startLSN
	//}
	//defer t.option.Adapter.Close()
	//
	//system
	//if res, err := t.result("IDENTIFY_SYSTEM;");err==nil {
	//	if len(res) > 0 {
	//		var lsnPos = util.MustString(res[0]["xlogpos"])
	//		if outputLSN, err := pgx.ParseLSN(lsnPos); err == nil {
	//			lsn = outputLSN
	//			t.log("startLSN:", lsn, lsnPos)
	//		}
	//	}
	//}
	// monitor table column update
	if t.option.MonitorUpdateColumn {
		for _, v := range t.option.Tables {
			if err := t.exec(fmt.Sprintf("ALTER TABLE %s REPLICA IDENTITY FULL;", v)); err != nil {
				return err
			}
		}
	}
	// create publication
	// 详见：select * from pg_catalog.pg_publication;
	if err = t.exec(fmt.Sprintf("CREATE PUBLICATION %s FOR %s", t.option.SlotName, t.option.PublicationTables())); err != nil {
		return
	}
	// start replication slot
	t._flushLsn, err = t.startReplication()
	if err != nil {
		return
	}
	// ready notify
	t.dmlHandler(ReplicationMessage{EventType: EventType_READY})
	// round read
	waitTimeout := 10 * time.Second
	for {
		var message *pgx.ReplicationMessage
		wctx, cancel := context.WithTimeout(ctx, waitTimeout)
		message, err = conn.WaitForReplicationMessage(wctx)
		cancel()
		if err == context.DeadlineExceeded {
			continue
		}
		if err != nil {
			return fmt.Errorf("replication failed: %s", err)
		}
		if message.WalMessage != nil {
			if err = t.handle(message.WalMessage); err != nil {
				return err
			}
		}
		// 服务器心跳验证当前sub是否可用
		// 不向master发送reply可能会导致连接EOF
		if message.ServerHeartbeat != nil {
			if message.ServerHeartbeat.ReplyRequested == 1 {
				if err = t.sendStatus(); err != nil {
					return err
				}
			}
		}
	}
}

func (t *ReplicationSyncer) exec(sql string) error {
	conn, err := t.conn()
	if err != nil {
		return err
	}
	t.log("exec:", sql)
	_, err = conn.Exec(sql)
	if err != nil {
		pgErr, ok := err.(pgx.PgError)
		if !ok || pgErr.Code != "42710" {
			return err
		}
	}
	return nil
}

func (t *ReplicationSyncer) result(sql string) (res []map[string]interface{}, err error) {
	conn, err := t.conn()
	if err != nil {
		return
	}
	t.log("query:", sql)
	rows, err := conn.Query(sql)
	if err != nil {
		return
	}
	res = make([]map[string]interface{}, 0)
	var values []interface{}
	for rows.Next() {
		values, err = rows.Values()
		fmt.Println(values, rows.FieldDescriptions())
		if err != nil {
			return
		}
		var item = make(map[string]interface{})
		for k, v := range rows.FieldDescriptions() {
			item[v.Name] = values[k]
		}
		res = append(res, item)
	}
	return
}

// 向master发送lsn，即：WAL中使用者已经收到解码数据的最新位置
// 详见：select * from pg_catalog.pg_replication_slots；结果中的confirmed_flush_lsn
func (t *ReplicationSyncer) sendStatus() error {
	lsn := t._flushLsn
	conn, err := t.conn()
	if err != nil {
		return err
	}
	k, err := pgx.NewStandbyStatus(lsn)
	if err != nil {
		return fmt.Errorf("error creating standby status: %s", err)
	}
	if err = conn.SendStandbyStatus(k); err != nil {
		return fmt.Errorf("failed to send standy status: %s", err)
	}
	t.log("sendStatus lsn:", lsn, pgx.FormatLSN(lsn))
	return nil
}

func (t *ReplicationSyncer) pluginArgs(version, publication string) []string {
	//} else if outputPlugin == "wal2json" {
	//	pluginArguments = []string{"\"pretty-print\" 'true'"}
	//}
	return []string{fmt.Sprintf(`proto_version '%s'`, version), fmt.Sprintf(`publication_names '%s'`, publication)}
}

// 开启replication slot
func (t *ReplicationSyncer) startReplication() (lsn uint64, err error) {
	conn, err := t.conn()
	if err != nil {
		return
	}
	if err = t.exec(fmt.Sprintf("CREATE_REPLICATION_SLOT %s LOGICAL %s", t.option.SlotName, "pgoutput")); err != nil {
		return
	}
	err = conn.StartReplication(t.option.SlotName, 0, -1, t.pluginArgs("1", t.option.SlotName)...)
	if err != nil {
		err = fmt.Errorf("failed to start replication: %s", err)
	}
	return
}

func (t *ReplicationSyncer) DropReplication() error {
	if err := t.exec(fmt.Sprintf("SELECT pg_drop_replication_slot('%s');", t.option.SlotName)); err != nil {
		return err
	}
	if err := t.exec(fmt.Sprintf("drop publication if exists %s;", t.option.SlotName)); err != nil {
		return err
	}
	return nil
}
