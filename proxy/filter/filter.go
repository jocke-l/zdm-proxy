package filter

import (
	"cloud-gate/migration/migration"
	"cloud-gate/proxy/cqlparser"
	"cloud-gate/updates"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	USE      = QueryType("USE")
	INSERT   = QueryType("INSERT")
	UPDATE   = QueryType("UPDATE")
	DELETE   = QueryType("DELETE")
	TRUNCATE = QueryType("TRUNCATE")
	PREPARE  = QueryType("PREPARE")
	MISC     = QueryType("MISC")

	// TODO: Finalize queue size to use
	queueSize = 1000

	cassHdrLen = 9
	cassMaxLen = 268435456 // 256 MB, per spec
)

type CQLProxy struct {
	SourceHostname   string
	SourceUsername   string
	SourcePassword   string
	SourcePort       int
	sourceHostString string

	AstraHostname   string
	AstraUsername   string
	AstraPassword   string
	AstraPort       int
	astraHostString string

	Port         int
	listeners    []net.Listener
	astraSession net.Conn

	queues      map[string]map[string]chan *Query
	queueLocks  map[string]map[string]*sync.Mutex
	queueSizes  map[string]map[string]int
	tablePaused map[string]map[string]bool

	// TODO: (maybe) create more locks to improve performance
	lock *sync.Mutex

	// Port to communicate with the migration service over
	MigrationPort int

	migrationComplete bool

	// Channel that signals that the migrator has finished the migration process.
	MigrationCompleteChan chan struct{}

	// Channel that signals that the migrator has begun the unloading/loading process
	MigrationStartChan chan *migration.Status
	migrationStatus    *migration.Status

	// Channel that the migration service will send us tables that have finished migrating
	// so that we can restart their queue consumption if it was paused
	TableMigratedChan chan *migration.Table

	// Is the proxy ready to process queries from user?
	ready bool

	// Channel signalling that the proxy is now ready to process queries
	ReadyChan chan struct{}

	// Number of open connections to the Client's Database
	connectionsToSource int

	// Channel to signal when the Proxy should stop all forwarding and close all connections
	ShutdownChan chan struct{}
	shutdown     bool

	// Channel to signal to coordinator that there are no more open connections to the Client's Database
	// and that the coordinator can redirect Envoy to point directly to Astra without any negative side effects
	ReadyForRedirect chan struct{}

	// Keeps track of the current keyspace queries are being ran in
	Keyspace string

	// Metrics
	Metrics Metrics

	// Struct that holds prepared queries by StreamID and by PreparedID
	preparedQueries *cqlparser.PreparedQueries
}

type QueryType string

type Query struct {
	Table *migration.Table

	Type  QueryType
	Query []byte
}

func (p *CQLProxy) Start() error {
	p.reset()

	// Attempt to connect to astra database using given credentials
	conn, err := connect(p.AstraHostname, p.AstraPort)
	if err != nil {
		return err
	}
	p.astraSession = conn

	go p.migrationLoop()

	err = p.listen(p.MigrationPort, p.handleMigrationCommunication)
	if err != nil {
		return err
	}

	err = p.listen(p.Port, p.handleDatabaseConnection)
	if err != nil {
		return err
	}

	return nil
}

// TODO: Maybe change migration_complete to migration_in_progress, so that we can turn on proxy before migration
// 	starts (if it ever starts), and it will just redirect to Astra normally.
func (p *CQLProxy) migrationLoop() {
	envVar := os.Getenv("migration_complete")
	status, err := strconv.ParseBool(envVar)
	if err != nil {
		log.Error(err)
	}
	p.migrationComplete = status && err == nil

	log.Debugf("Migration Complete: %t", p.migrationComplete)

	if !p.migrationComplete {
		log.Info("Proxy waiting for migration start signal.")
		for {
			select {
			case status := <-p.MigrationStartChan:
				p.loadMigrationInfo(status)
				log.Info("Proxy ready to consume queries.")

			case table := <-p.TableMigratedChan:
				p.startTable(table.Keyspace, table.Name)
				log.Debugf("Restarted query consumption on table %s.%s", table.Keyspace, table.Name)

			case <-p.MigrationCompleteChan:
				p.migrationComplete = true
				log.Info("Migration Complete. Directing all new connections to Astra Database.")

			case <-p.ShutdownChan:
				p.Shutdown()
				return
			}
		}
	}
}

func (p *CQLProxy) loadMigrationInfo(status *migration.Status) {
	for keyspace, tables := range status.Tables {
		p.queues[keyspace] = make(map[string]chan *Query)
		p.queueLocks[keyspace] = make(map[string]*sync.Mutex)
		p.queueSizes[keyspace] = make(map[string]int)
		p.tablePaused[keyspace] = make(map[string]bool)
		for tableName := range tables {
			p.queues[keyspace][tableName] = make(chan *Query, queueSize)
			p.queueLocks[keyspace][tableName] = &sync.Mutex{}

			go p.consumeQueue(keyspace, tableName)
		}
	}

	p.migrationStatus = status
	p.ReadyChan <- struct{}{}
	p.ready = true

	log.Info("Proxy ready to execute queries.")
}

func (p *CQLProxy) listen(port int, handler func(net.Conn)) error {
	l, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		log.Error(err)
		return err
	}

	p.lock.Lock()
	p.listeners = append(p.listeners, l)
	p.lock.Unlock()

	go func() {
		defer l.Close()
		for {
			conn, err := l.Accept()
			if err != nil {
				if p.shutdown {
					log.Infof("Shutting down listener %v", l)
					return
				}
				log.Error(err)
				continue
			}
			go handler(conn)
		}
	}()

	return nil
}

func (p *CQLProxy) handleDatabaseConnection(conn net.Conn) {
	hostname := p.sourceHostString
	if p.migrationComplete {
		hostname = p.astraHostString
	}

	dst, err := net.Dial("tcp", hostname)
	if err != nil {
		log.Error(err)
		return
	}

	if hostname == p.sourceHostString {
		p.incrementSources()
	}

	// Begin two way packet forwarding
	go p.forward(conn, dst)
	go p.forward(dst, conn)

}

func (p *CQLProxy) handleMigrationCommunication(conn net.Conn) {
	defer conn.Close()

	// TODO: change buffer size
	buf := make([]byte, 0xfffffff)
	for {
		bytesRead, err := conn.Read(buf)
		if err != nil {
			if err == io.EOF {
				log.Error(err)
			}
			return
		}

		b := buf[:bytesRead]
		var update updates.Update
		err = json.Unmarshal(b, &update)
		if err != nil {
			log.Error(err)
			return
		}

		err = p.handleUpdate(&update)
		if err != nil {
			log.Error(err)
			return
		}

		_, err = conn.Write(b)
		if err != nil {
			log.Error(err)
			continue
		}

	}

}

func (p *CQLProxy) handleUpdate(req *updates.Update) error {
	switch req.Type {
	case updates.Start:
		var status migration.Status
		err := json.Unmarshal(req.Data, &status)
		if err != nil {
			return err
		}

		p.MigrationStartChan <- &status
	case updates.TableUpdate:
		var table migration.Table
		err := json.Unmarshal(req.Data, &table)
		if err != nil {
			return err
		}
		p.migrationStatus.Lock.Lock()
		p.migrationStatus.Tables[table.Keyspace][table.Name] = &table
		p.migrationStatus.Lock.Unlock()
	case updates.Complete:
		p.MigrationCompleteChan <- struct{}{}
	case updates.Shutdown:
		p.ShutdownChan <- struct{}{}
	}
	return nil
}

func (p *CQLProxy) forward(src, dst net.Conn) {
	defer src.Close()
	defer dst.Close()

	if dst.RemoteAddr().String() == p.sourceHostString {
		defer p.decrementSources()
	}

	// TODO: Finalize buffer size
	// 	Right now just using 0xffff as a placeholder, but the maximum request
	// 	that could be sent through the CQL wire protocol is 256mb, so we should accommodate that, unless there's
	// 	an issue with that
	buf := make([]byte, 0xffff)
	data := make([]byte, 0)
	for {
		bytesRead, err := src.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Debugf("%s disconnected", src.RemoteAddr())
			} else {
				log.Error(err)
			}
			return
		}
		data = append(data, buf[:bytesRead]...)

		// Build queries reading in at most 0xffff size at a time.
		for true {
			//if there is not a full CQL header
			if len(data) < cassHdrLen {
				break
			}

			bodyLength := binary.BigEndian.Uint32(data[5:9])
			fullLength := cassHdrLen + int(bodyLength)
			if len(data) < fullLength || len(data) > cassMaxLen {
				break
			}

			query := data[:fullLength]
			_, err := dst.Write(query)
			if err != nil {
				log.Error(err)
				continue
			}

			// We only want to mirror writes if this connection is still directly connected to the
			// client source Database
			if dst.RemoteAddr().String() == p.sourceHostString {
				// Passes all data along to be separated into requests and responses
				err := p.mirrorData(query)
				if err != nil {
					log.Error(err)
				}
			}

			p.Metrics.incrementPackets()

			// Keep any extra bytes in the buffer that is part of the next query
			data = data[fullLength:]
		}
	}
}

// MirrorData receives all data and decides what to do
func (p *CQLProxy) mirrorData(data []byte) error {
	compressionFlag := data[1] & 0x01
	if compressionFlag == 1 {
		return errors.New("compression flag set, unable to parse reply beyond header")
	}

	// if reply, we parse replies but only look for prepared-query-id responses
	if data[0] > 0x80 {
		cqlparser.CassandraParseReply(p.preparedQueries, data)
		return nil
	}

	// Returns list of []string paths in form /opcode/action/table
	// opcode is "startup", "query", "batch", etc.
	// action is "select", "insert", "update", etc,
	// table is the table as written in the command
	paths, err := cqlparser.CassandraParseRequest(p.preparedQueries, data)
	if err != nil {
		return err
	}

	if len(paths) == 0 {
		return errors.New("length 0 request")
	}

	// FIXME: Handle more actions based on paths
	// currently handles batch, query, and prepare statements that involve 'use, insert, update, delete, and truncate'
	if len(paths) > 1 {
		return nil
		// return p.handleBatchQuery(data, paths)
		// TODO: Handle batch statements
	} else {
		if paths[0] == cqlparser.UnknownPreparedQueryPath {
			log.Debug("Err: Encountered unknown prepared query. Query Ignored")
			return nil
		}

		fields := strings.Split(paths[0], "/")

		if len(fields) > 2 {
			if fields[1] == "prepare" {
				q := &Query{
					Table: nil,
					Type:  PREPARE,
					Query: data}
				return p.execute(q)
			} else if fields[1] == "query" || fields[1] == "execute" {
				keyspace, table := extractTableInfo(fields[3])
				if keyspace == "" {
					keyspace = p.Keyspace
				}

				switch fields[2] {
				case "use":
					return p.handleUseQuery(data, fields[3])
				case "insert":
					return p.handleInsertQuery(data, keyspace, table)
				case "update":
					return p.handleUpdateQuery(data, keyspace, table)
				case "delete":
					return p.handleDeleteQuery(data, keyspace, table)
				case "truncate":
					return p.handleTruncateQuery(data, keyspace, table)
				case "select":
					p.Metrics.incrementReads()
				}
			}
		} else {
			// path is '/opcode' case
			// FIXME: decide if there are any cases we need to handle here
			q := &Query{
				Table: nil,
				Type:  MISC,
				Query: data}
			return p.execute(q)
		}

	}
	return nil
}

func (p *CQLProxy) handleUseQuery(query []byte, keyspace string) error {

	// Cassandra assumes case-insensitive unless keyspace is encased in quotation marks
	if strings.HasPrefix(keyspace, "\"") && strings.HasSuffix(keyspace, "\"") {
		keyspace = keyspace[1 : len(keyspace)-1]
	} else {
		keyspace = strings.ToLower(keyspace)
	}

	if _, ok := p.migrationStatus.Tables[keyspace]; !ok {
		return errors.New("invalid keyspace")
	}

	p.Keyspace = keyspace

	q := &Query{
		Table: nil,
		Type:  USE,
		Query: query}

	return p.execute(q)
}

func (p *CQLProxy) handleTruncateQuery(query []byte, keyspace string, tableName string) error {
	table, ok := p.migrationStatus.Tables[keyspace][tableName]
	if !ok {
		return fmt.Errorf("table %s.%s does not exist", keyspace, tableName)
	}

	if !p.tablePaused[keyspace][tableName] && p.tableStatus(keyspace, tableName) != migration.LoadingDataComplete {
		p.stopTable(keyspace, tableName)
	}

	q := &Query{
		Table: table,
		Type:  TRUNCATE,
		Query: query}

	p.queueQuery(q)

	return nil
}

func (p *CQLProxy) handleDeleteQuery(query []byte, keyspace string, tableName string) error {
	table, ok := p.migrationStatus.Tables[keyspace][tableName]
	if !ok {
		return fmt.Errorf("table %s.%s does not exist", keyspace, tableName)
	}

	// Wait for migration of table to be finished before processing anymore queries
	if !p.tablePaused[keyspace][tableName] && p.tableStatus(keyspace, tableName) != migration.LoadingDataComplete {
		p.stopTable(keyspace, tableName)
	}

	q := &Query{
		Table: table,
		Type:  DELETE,
		Query: query}

	p.queueQuery(q)

	return nil
}

// Extract table name from insert query & add query to proper queue
func (p *CQLProxy) handleInsertQuery(query []byte, keyspace string, tableName string) error {
	table, ok := p.migrationStatus.Tables[keyspace][tableName]
	if !ok {
		return fmt.Errorf("table %s.%s does not exist", keyspace, tableName)
	}

	q := &Query{
		Table: table,
		Type:  INSERT,
		Query: query}

	p.queueQuery(q)

	return nil
}

// Extract table name from update query & add query to proper queue
func (p *CQLProxy) handleUpdateQuery(query []byte, keyspace string, tableName string) error {
	table, ok := p.migrationStatus.Tables[keyspace][tableName]
	if !ok {
		return fmt.Errorf("table %s.%s does not exist", keyspace, tableName)
	}

	// Wait for migration of table to be finished before processing anymore queries
	if !p.tablePaused[keyspace][tableName] && p.tableStatus(keyspace, tableName) != migration.LoadingDataComplete {
		p.stopTable(keyspace, tableName)
	}

	q := &Query{
		Table: table,
		Type:  UPDATE,
		Query: query}

	p.queueQuery(q)

	return nil
}

//TODO: Handle batch statements
func (p *CQLProxy) handleBatchQuery(query []byte, paths []string) error {
	return nil
}

func (p *CQLProxy) queueQuery(query *Query) {
	p.queues[query.Table.Keyspace][query.Table.Name] <- query

	p.lock.Lock()
	defer p.lock.Unlock()
	p.queueSizes[query.Table.Keyspace][query.Table.Name]++
}

func (p *CQLProxy) consumeQueue(keyspace string, table string) {
	log.Debugf("Beginning consumption of queries for %s.%s", keyspace, table)

	for {
		select {
		case query := <-p.queues[keyspace][table]:
			p.queueLocks[keyspace][table].Lock()

			// Driver is async, so we don't need a lock around query execution
			err := p.execute(query)
			if err != nil {
				// TODO: Figure out exactly what to do if we're unable to write
				// 	If it's a bad query, no issue, but if it's a good query that isn't working for some reason
				// 	we need to figure out what to do
				log.Error(err)

				p.Metrics.incrementWriteFails()
			} else {
				p.Metrics.incrementWrites()
			}

			p.lock.Lock()
			p.queueSizes[keyspace][table]--
			p.lock.Unlock()

			p.queueLocks[keyspace][table].Unlock()
		}

	}
}

// TODO: Add exponential backoff
func (p *CQLProxy) execute(query *Query) error {
	log.Debugf("Executing %v", *query)

	var err error
	for i := 1; i <= 5; i++ {
		// TODO: Catch reply and see if it was successful
		_, err := p.astraSession.Write(query.Query)
		if err == nil {
			break
		}

		time.Sleep(500 * time.Millisecond)
		log.Debugf("Retrying %s attempt #%d", query, i+1)
	}

	return err
}

func (p *CQLProxy) tableStatus(keyspace string, tableName string) migration.Step {
	table := p.migrationStatus.Tables[keyspace][tableName]
	table.Lock.Lock()
	defer table.Lock.Unlock()

	status := table.Step
	return status
}

func (p *CQLProxy) stopTable(keyspace string, tableName string) {
	log.Debugf("Stopping query consumption on %s.%s", keyspace, tableName)
	p.tablePaused[keyspace][tableName] = true
	p.queueLocks[keyspace][tableName].Lock()
}

func (p *CQLProxy) startTable(keyspace string, tableName string) {
	log.Debugf("Starting query consumption on %s.%s", keyspace, tableName)
	p.tablePaused[keyspace][tableName] = false
	p.queueLocks[keyspace][tableName].Unlock()
}

func (p *CQLProxy) incrementSources() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.connectionsToSource++
}

func (p *CQLProxy) decrementSources() {
	p.lock.Lock()
	defer p.lock.Unlock()

	p.connectionsToSource--

	if p.migrationComplete && p.connectionsToSource == 0 {
		log.Debug("No more connections to client database; ready for redirect.")
		p.ReadyForRedirect <- struct{}{}
	}
}

func (p *CQLProxy) Shutdown() {
	log.Info("Proxy shutting down...")
	p.shutdown = true
	for _, listener := range p.listeners {
		listener.Close()
	}

	// TODO: Stop all goroutines
}

func (p *CQLProxy) reset() {
	p.queues = make(map[string]map[string]chan *Query)
	p.queueLocks = make(map[string]map[string]*sync.Mutex)
	p.queueSizes = make(map[string]map[string]int)
	p.tablePaused = make(map[string]map[string]bool)
	p.ready = false
	p.ReadyChan = make(chan struct{})
	p.ShutdownChan = make(chan struct{})
	p.shutdown = false
	p.listeners = []net.Listener{}
	p.ReadyForRedirect = make(chan struct{})
	p.connectionsToSource = 0
	p.lock = &sync.Mutex{}
	p.Metrics = Metrics{}
	p.Metrics.lock = &sync.Mutex{}
	p.sourceHostString = fmt.Sprintf("%s:%d", p.SourceHostname, p.SourcePort)
	p.astraHostString = fmt.Sprintf("%s:%d", p.AstraHostname, p.AstraPort)
	p.preparedQueries = &cqlparser.PreparedQueries{
		PreparedQueryPathByStreamID:   make(map[uint16]string),
		PreparedQueryPathByPreparedID: make(map[string]string),
	}
}

// TODO: Maybe add a couple retries, or let the caller deal with that?
func connect(hostname string, port int) (net.Conn, error) {
	astraHostString := fmt.Sprintf("%s:%d", hostname, port)
	dst, err := net.Dial("tcp", astraHostString)
	return dst, err
}

// Given a FROM argument, extract the table name
// ex: table, keyspace.table, keyspace.table;, keyspace.table(, etc..
func extractTableInfo(fromClause string) (string, string) {
	var keyspace string

	// Remove keyspace if table in format keyspace.table
	if i := strings.IndexRune(fromClause, '.'); i != -1 {
		keyspace = fromClause[:i]
	}

	tableName := fromClause

	// Remove semicolon if it is attached to the table name from the query
	if i := strings.IndexRune(tableName, ';'); i != -1 {
		tableName = tableName[:i]
	}

	// Remove keyspace if table in format keyspace.table
	if i := strings.IndexRune(tableName, '.'); i != -1 {
		tableName = tableName[i+1:]
	}

	// Remove column names if part of an INSERT query: ex: TABLE(col, col)
	if i := strings.IndexRune(tableName, '('); i != -1 {
		tableName = tableName[:i]
	}

	return keyspace, tableName
}

type Metrics struct {
	PacketCount int
	Reads       int
	Writes      int

	WriteFails int
	ReadFails  int

	lock *sync.Mutex
}

func (m *Metrics) incrementPackets() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.PacketCount++
}

func (m *Metrics) incrementReads() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.Reads++
}

func (m *Metrics) incrementWrites() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.Writes++
}

func (m *Metrics) incrementWriteFails() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.WriteFails++
}

func (m *Metrics) incrementReadFails() {
	m.lock.Lock()
	defer m.lock.Unlock()

	m.ReadFails++
}
