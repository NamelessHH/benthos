package reader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Jeffail/gabs"

	"github.com/Jeffail/benthos/lib/log"
	"github.com/Jeffail/benthos/lib/message"
	"github.com/Jeffail/benthos/lib/metrics"
	"github.com/Jeffail/benthos/lib/types"
	"github.com/siddontang/go-mysql/canal"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go-mysql/schema"
)

//------------------------------------------------------------------------------

// MySQLConfig contains configuration fields for the MySQL input type.
type MySQLConfig struct {
	BatchSize     int      `json:"batch_size" yaml:"batch_size"`
	BufferTimeout string   `json:"buffer_timeout" yaml:"buffer_timeout"`
	Cache         string   `json:"cache" yaml:"cache"`
	ConsumerID    uint32   `json:"consumer_id" yaml:"consumer_id"`
	Databases     []string `json:"databases" yaml:"databases"`
	Host          string   `json:"host" yaml:"host"`
	KeyPrefix     string   `json:"key_prefix" yaml:"key_prefix"`
	Latest        bool     `json:"latest" yaml:"latest"`
	MySQLDumpPath string   `json:"mysqldump_path" yaml:"mysqldump_path"`
	Password      string   `json:"password" yaml:"password"`
	PrefetchCount uint     `json:"prefetch_count" yaml:"prefetch_count"`
	Port          uint32   `json:"port" yaml:"port"`
	Tables        []string `json:"tables" yaml:"tables"`
	Username      string   `json:"username" yaml:"username"`
}

// NewMySQLConfig creates a new MySQLConfig with default values
func NewMySQLConfig() MySQLConfig {
	return MySQLConfig{
		Host: "localhost",
		Port: 3306,
	}
}

//------------------------------------------------------------------------------

// MySQL is an input type that reads from a MySQL binary log stream.
type MySQL struct {
	sync.RWMutex
	canal.DummyEventHandler

	canal  *canal.Canal
	pos    mysqlPosition
	synced bool
	key    string

	batchSize        int
	bufferTimeout    time.Duration
	internalMessages chan *canal.RowsEvent
	interruptChan    chan struct{}
	failedMessage    *canal.RowsEvent
	closed           chan error

	conf  MySQLConfig
	cache types.Cache
	stats metrics.Type
	log   log.Modular
}

// NewMySQL creates a new MySQL input type.
func NewMySQL(conf MySQLConfig, cache types.Cache, log log.Modular, stats metrics.Type) (*MySQL, error) {
	// create base reader
	m := MySQL{
		key:              fmt.Sprintf("%s%d", conf.KeyPrefix, conf.ConsumerID),
		batchSize:        conf.BatchSize,
		internalMessages: make(chan *canal.RowsEvent, conf.PrefetchCount),
		interruptChan:    make(chan struct{}),
		closed:           make(chan error),
		conf:             conf,
		cache:            cache,
		stats:            stats,
		log:              log.NewModule(".input.mysql"),
	}

	dur := conf.BufferTimeout
	if dur == "" {
		dur = "1s"
	}
	timeout, err := time.ParseDuration(dur)
	if err != nil {
		return nil, err
	}
	m.bufferTimeout = timeout

	// build binlog consumer config
	c := canal.NewDefaultConfig()
	c.Addr = fmt.Sprintf("%s:%d", conf.Host, conf.Port)
	c.User = conf.Username
	c.Password = conf.Password
	c.Dump.DiscardErr = false
	c.Dump.ExecutionPath = conf.MySQLDumpPath
	c.Dump.SkipMasterData = false
	if len(conf.Databases) == 1 && len(conf.Tables) > 0 {
		c.Dump.TableDB = conf.Databases[0]
		c.Dump.Tables = conf.Tables
	} else {
		c.Dump.Databases = conf.Databases
	}

	// create binlog consumer client
	client, err := canal.NewCanal(c)
	if err != nil {
		return nil, fmt.Errorf("error creating mysql binlog client: %v", err)
	}
	client.SetEventHandler(&m)
	m.canal = client

	return &m, nil
}

//------------------------------------------------------------------------------

// OnPosSynced handles a MySQL binlog position event
func (m *MySQL) OnPosSynced(pos mysql.Position, force bool) error {
	// update state
	m.Lock()
	m.pos = mysqlPosition{
		ConsumerID: m.conf.ConsumerID,
		Log:        pos.Name,
		Position:   pos.Pos,
	}
	m.synced = false
	m.Unlock()

	// sync if force flag is true
	if force == true {
		return m.Acknowledge(nil)
	}

	return nil
}

// OnRow handles a MySQL binlog row event
func (m *MySQL) OnRow(e *canal.RowsEvent) error {
	select {
	case m.internalMessages <- e:
	case <-m.interruptChan:
	}
	return nil
}

//------------------------------------------------------------------------------

// Acknowledge attempts to synchronize the current reader state with the backend
func (m *MySQL) Acknowledge(err error) error {
	if err != nil {
		return err
	}

	m.Lock()
	defer m.Unlock()
	if m.synced == true {
		return nil
	}

	m.pos.LastSyncedAt = time.Now()
	pos, err := json.Marshal(m.pos)
	if err != nil {
		return fmt.Errorf("error marshalling mysql position: %v", err)
	}

	if err := m.cache.Set(m.key, pos); err != nil {
		return fmt.Errorf("error syncing mysql position: %v", err)
	}

	m.synced = true
	return nil
}

// CloseAsync shuts down the MySQL input and stops processing requests.
func (m *MySQL) CloseAsync() {
	close(m.interruptChan)
}

// Connect retrieves the starting binlog position and establishes a connection
// with MySQL
func (m *MySQL) Connect() error {
	m.Lock()
	defer m.Unlock()

	// load starting position
	pos, err := m.loadPosition()
	if err != nil {
		return fmt.Errorf("unable to load mysql binlog position: %v", err)
	}

	// start the binlog consumer
	var start func(c *canal.Canal) error
	if pos == nil {
		start = func(c *canal.Canal) error {
			return c.Run()
		}
	} else {
		start = func(c *canal.Canal) error {
			return c.RunFrom(mysql.Position{
				Name: pos.Log,
				Pos:  pos.Position,
			})
		}
	}
	go func() {
		m.closed <- start(m.canal)
	}()

	return nil
}

// loadPosition loads the latest binlog position
func (m *MySQL) loadPosition() (*mysqlPosition, error) {
	var pos mysqlPosition

	state, err := m.cache.Get(m.key)
	if err != nil {
		m.log.Debugf("error retrieving last synchronized mysql position: %v", err)
	}
	if err := json.Unmarshal(state, &pos); err != nil {
		if !m.conf.Latest {
			return nil, fmt.Errorf("error loading mysql position: %v", err)
		}
		p, err := m.canal.GetMasterPos()
		if err != nil {
			return nil, fmt.Errorf("error retrieving latest mysql position: %v", err)
		}
		pos.ConsumerID = m.conf.ConsumerID
		pos.Log = p.Name
		pos.Position = p.Pos
	}

	return &pos, nil
}

// marshalKeys computes a map of primary key columns to values
func (m *MySQL) marshalKeys(e *canal.RowsEvent, summary *MySQLRowSummary) (map[string]interface{}, error) {
	// grab a reference to appropriate source image
	var src []byte
	switch e.Action {
	case canal.InsertAction:
		src = summary.After
	case canal.UpdateAction:
		src = summary.After
	case canal.DeleteAction:
		src = summary.Before
	}
	image, err := gabs.ParseJSON(src)
	if err != nil {
		return nil, err
	}

	keys := make(map[string]interface{})
	for i := range e.Table.PKColumns {
		col := e.Table.GetPKColumn(i)
		keys[col.Name] = image.S(col.Name).Data()
	}
	return keys, nil
}

// marshalRowSummary converts a row image to json
func (m *MySQL) marshalRowSummary(table *schema.Table, row []interface{}) []byte {
	result := gabs.New()
	for i, c := range table.Columns {
		result.Set(m.parseValue(&c, row[i]), c.Name)
	}
	return result.Bytes()
}

// parse a binlog event into a json byte slice
func (m *MySQL) parse(e *canal.RowsEvent, log string) (*MysqlMessage, error) {
	msg := MysqlMessage{
		Row:       m.parseRowSummary(e),
		Schema:    e.Table.Schema,
		Table:     e.Table.Name,
		Timestamp: time.Unix(int64(e.Header.Timestamp), 0),
		Type:      e.Action,
	}

	keys, err := m.marshalKeys(e, &msg.Row)
	if err != nil {
		return nil, err
	}
	msg.Key = keys

	var id bytes.Buffer
	fmt.Fprintf(&id, "%s:%d:%s:%s:", log, e.Header.LogPos, msg.Schema, msg.Table)
	for _, v := range keys {
		fmt.Fprintf(&id, "%v:", v)
	}
	msg.ID = strings.TrimSuffix(id.String(), ":")

	return &msg, nil
}

// parseRowSummary parses the before and/or after row image
func (m *MySQL) parseRowSummary(e *canal.RowsEvent) MySQLRowSummary {
	var summary MySQLRowSummary
	switch e.Action {
	case canal.UpdateAction:
		summary.Before = m.marshalRowSummary(e.Table, e.Rows[0])
		summary.After = m.marshalRowSummary(e.Table, e.Rows[1])
	case canal.InsertAction:
		summary.After = m.marshalRowSummary(e.Table, e.Rows[0])
	case canal.DeleteAction:
		summary.Before = m.marshalRowSummary(e.Table, e.Rows[0])
	}
	return summary
}

// parseValue value using table column definition
// borrowed from https://github.com/siddontang/go-mysql-elasticsearch/blob/master/river/sync.go#L261
func (m *MySQL) parseValue(col *schema.TableColumn, value interface{}) interface{} {
	switch col.Type {
	case schema.TYPE_ENUM:
		switch value := value.(type) {
		case int64:
			// for binlog, ENUM may be int64, but for dump, enum is string
			eNum := value - 1
			if eNum < 0 || eNum >= int64(len(col.EnumValues)) {
				// we insert invalid enum value before, so return empty
				m.log.Warnf("invalid binlog enum index %d, for enum %v", eNum, col.EnumValues)
				return ""
			}
			return col.EnumValues[eNum]
		}
	case schema.TYPE_SET:
		switch value := value.(type) {
		case int64:
			// for binlog, SET may be int64, but for dump, SET is string
			bitmask := value
			sets := make([]string, 0, len(col.SetValues))
			for i, s := range col.SetValues {
				if bitmask&int64(1<<uint(i)) > 0 {
					sets = append(sets, s)
				}
			}
			return strings.Join(sets, ",")
		}
	case schema.TYPE_BIT:
		switch value := value.(type) {
		case string:
			// for binlog, BIT is int64, but for dump, BIT is string
			// for dump 0x01 is for 1, \0 is for 0
			if value == "\x01" {
				return int64(1)
			}

			return int64(0)
		}
	case schema.TYPE_STRING:
		switch value := value.(type) {
		case []byte:
			return string(value[:])
		}
	case schema.TYPE_JSON:
		var f interface{}
		var err error
		switch v := value.(type) {
		case string:
			err = json.Unmarshal([]byte(v), &f)
		case []byte:
			err = json.Unmarshal(v, &f)
		}
		if err == nil && f != nil {
			return f
		}
	case schema.TYPE_DATETIME:
		switch v := value.(type) {
		case string:
			vt, _ := time.ParseInLocation(mysql.TimeFormat, string(v), time.UTC)
			return vt.Format(time.RFC3339Nano)
		}
	}
	return value
}

// Read attempts to read a new message from MySQL.
func (m *MySQL) Read() (types.Message, error) {
	m.RLock()
	log := m.pos.Log
	m.RUnlock()

	msg := message.New(nil)
	timeout := time.After()
	for {

	}
	// check for failed message to retry, otherwise block until
	// the next binlog row event is available
	var e *canal.RowsEvent
	if m.failedMessage != nil {
		m.Lock()
		e = m.failedMessage
		m.failedMessage = nil
		m.Unlock()
	} else {
		select {
		case e = <-m.internalMessages:
		case <-m.interruptChan:
			return nil, types.ErrTypeClosed
		}
	}

	// parse the binlog row event, requeue it if there is an error
	record, err := m.parse(e, log)
	if err != nil {
		m.Lock()
		m.failedMessage = e
		m.Unlock()
		m.log.Errorf("failed to parse binlog row event: %v", err)
		return nil, types.ErrBadMessageBytes
	}

	var part message.Part
	if err := part.SetJSON(record); err != nil {
		return nil, err
	}

	msg := message.New(nil)
	msg.Append(&part)
	return msg, nil
}

// WaitForClose blocks until the MySQL input has closed down.
func (m *MySQL) WaitForClose(timeout time.Duration) error {
	m.canal.Close()
	err := <-m.closed
	if err.Error() == context.Canceled.Error() {
		err = nil
	}
	return m.Acknowledge(err)
}

//------------------------------------------------------------------------------

// MysqlMessage represents a single mysql binlog row event
type MysqlMessage struct {
	ID        string                 `json:"id"`
	Key       map[string]interface{} `json:"key"`
	Row       MySQLRowSummary        `json:"row"`
	Schema    string                 `json:"schema"`
	Table     string                 `json:"table"`
	Timestamp time.Time              `json:"timestamp"`
	Type      string                 `json:"type"`
}

// MySQLRowSummary contains the before and after row images of a single
// binlog row event
type MySQLRowSummary struct {
	After  json.RawMessage `json:"after"`
	Before json.RawMessage `json:"before"`
}

// mysqlPosition describes an individual reader's binlog position at a given point in time
type mysqlPosition struct {
	ConsumerID   uint32    `db:"consumer_id" dynamodbav:"consumer_id"`
	LastSyncedAt time.Time `db:"synced_at" dynamodbav:"sync_at"`
	Log          string    `db:"log" dynamodbav:"log"`
	Position     uint32    `db:"position" dynamodbav:"position"`
}
