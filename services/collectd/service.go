// Package collectd provides a service for InfluxDB to ingest data via the collectd protocol.
package collectd // import "github.com/influxdata/influxdb/services/collectd"

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"collectd.org/api"
	"collectd.org/network"
	"github.com/influxdata/influxdb/models"
	"github.com/influxdata/influxdb/services/meta"
	"github.com/influxdata/influxdb/tsdb"
)

// statistics gathered by the collectd service.
const (
	statPointsReceived       = "pointsRx"
	statBytesReceived        = "bytesRx"
	statPointsParseFail      = "pointsParseFail"
	statReadFail             = "readFail"
	statBatchesTransmitted   = "batchesTx"
	statPointsTransmitted    = "pointsTx"
	statBatchesTransmitFail  = "batchesTxFail"
	statDroppedPointsInvalid = "droppedPointsInvalid"
)

// pointsWriter is an internal interface to make testing easier.
type pointsWriter interface {
	WritePointsPrivileged(database, retentionPolicy string, consistencyLevel models.ConsistencyLevel, points []models.Point) error
}

// metaClient is an internal interface to make testing easier.
type metaClient interface {
	CreateDatabase(name string) (*meta.DatabaseInfo, error)
}

// TypesDBFile reads a collectd types db from a file.
func TypesDBFile(path string) (typesdb *api.TypesDB, err error) {
	var reader *os.File
	reader, err = os.Open(path)
	if err == nil {
		typesdb, err = api.NewTypesDB(reader)
	}
	return
}

type Diagnostic interface {
	Starting()
	Listening(addr net.Addr)
	Closed()
	UnableToReadDirectory(path string, err error)
	LoadingPath(path string)
	TypesParseError(path string, err error)

	ReadFromUDPError(err error)
	ParseError(err error)
	DroppingPoint(name string, err error)
	InternalStorageCreateError(err error)
	PointWriterError(database string, err error)
}

// Service represents a UDP server which receives metrics in collectd's binary
// protocol and stores them in InfluxDB.
type Service struct {
	Config       *Config
	MetaClient   metaClient
	PointsWriter pointsWriter
	Diagnostic   Diagnostic

	wg      sync.WaitGroup
	conn    *net.UDPConn
	batcher *tsdb.PointBatcher
	popts   network.ParseOpts
	addr    net.Addr

	mu    sync.RWMutex
	ready bool          // Has the required database been created?
	done  chan struct{} // Is the service closing or closed?

	// expvar-based stats.
	stats       *Statistics
	defaultTags models.StatisticTags
}

// NewService returns a new instance of the collectd service.
func NewService(c Config) *Service {
	s := Service{
		// Use defaults where necessary.
		Config: c.WithDefaults(),

		stats:       &Statistics{},
		defaultTags: models.StatisticTags{"bind": c.BindAddress},
	}

	return &s
}

// Open starts the service.
func (s *Service) Open() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.closed() {
		return nil // Already open.
	}
	s.done = make(chan struct{})

	if s.Diagnostic != nil {
		s.Diagnostic.Starting()
	}

	if s.Config.BindAddress == "" {
		return fmt.Errorf("bind address is blank")
	} else if s.Config.Database == "" {
		return fmt.Errorf("database name is blank")
	} else if s.PointsWriter == nil {
		return fmt.Errorf("PointsWriter is nil")
	}

	if s.popts.TypesDB == nil {
		// Open collectd types.
		if stat, err := os.Stat(s.Config.TypesDB); err != nil {
			return fmt.Errorf("Stat(): %s", err)
		} else if stat.IsDir() {
			alltypesdb, err := api.NewTypesDB(&bytes.Buffer{})
			if err != nil {
				return err
			}
			var readdir func(path string)
			readdir = func(path string) {
				files, err := ioutil.ReadDir(path)
				if err != nil {
					if s.Diagnostic != nil {
						s.Diagnostic.UnableToReadDirectory(path, err)
					}
					return
				}

				for _, f := range files {
					fullpath := filepath.Join(path, f.Name())
					if f.IsDir() {
						readdir(fullpath)
						continue
					}

					if s.Diagnostic != nil {
						s.Diagnostic.LoadingPath(fullpath)
					}
					types, err := TypesDBFile(fullpath)
					if err != nil {
						continue
					}

					alltypesdb.Merge(types)
				}
			}
			readdir(s.Config.TypesDB)
			s.popts.TypesDB = alltypesdb
		} else {
			if s.Diagnostic != nil {
				s.Diagnostic.LoadingPath(s.Config.TypesDB)
			}
			types, err := TypesDBFile(s.Config.TypesDB)
			if err != nil {
				return fmt.Errorf("Open(): %s", err)
			}
			s.popts.TypesDB = types
		}
	}

	// Sets the security level according to the config.
	// Default not necessary because we validate the config.
	switch s.Config.SecurityLevel {
	case "none":
		s.popts.SecurityLevel = network.None
	case "sign":
		s.popts.SecurityLevel = network.Sign
	case "encrypt":
		s.popts.SecurityLevel = network.Encrypt
	}

	// Sets the auth file according to the config.
	if s.popts.PasswordLookup == nil {
		s.popts.PasswordLookup = network.NewAuthFile(s.Config.AuthFile)
	}

	// Resolve our address.
	addr, err := net.ResolveUDPAddr("udp", s.Config.BindAddress)
	if err != nil {
		return fmt.Errorf("unable to resolve UDP address: %s", err)
	}
	s.addr = addr

	// Start listening
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("unable to listen on UDP: %s", err)
	}

	if s.Config.ReadBuffer != 0 {
		err = conn.SetReadBuffer(s.Config.ReadBuffer)
		if err != nil {
			return fmt.Errorf("unable to set UDP read buffer to %d: %s",
				s.Config.ReadBuffer, err)
		}
	}
	s.conn = conn

	if s.Diagnostic != nil {
		s.Diagnostic.Listening(conn.LocalAddr())
	}

	// Start the points batcher.
	s.batcher = tsdb.NewPointBatcher(s.Config.BatchSize, s.Config.BatchPending, time.Duration(s.Config.BatchDuration))
	s.batcher.Start()

	// Create waitgroup for signalling goroutines to stop and start goroutines
	// that process collectd packets.
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.serve() }()
	go func() { defer s.wg.Done(); s.writePoints() }()

	return nil
}

// Close stops the service.
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed() {
		return nil // Already closed.
	}
	close(s.done)

	// Close the connection, and wait for the goroutine to exit.
	if s.conn != nil {
		s.conn.Close()
	}
	if s.batcher != nil {
		s.batcher.Stop()
	}
	s.wg.Wait()

	// Release all remaining resources.
	s.conn = nil
	s.batcher = nil
	if s.Diagnostic != nil {
		s.Diagnostic.Closed()
	}
	s.done = nil
	return nil
}

func (s *Service) closed() bool {
	select {
	case <-s.done:
		// Service is closing.
		return true
	default:
	}
	return s.done == nil
}

// Ready returns true if the internal storage has been created successfully.
func (s *Service) Ready() bool {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()
	return ready
}

// Batcher returns the incoming point batcher for this service.
// This is primarily meant to be used for tests, but could also be used for manually
// inserting points into the collectd database.
func (s *Service) Batcher() *tsdb.PointBatcher {
	return s.batcher
}

// createInternalStorage ensures that the required database has been created.
func (s *Service) createInternalStorage() error {
	s.mu.RLock()
	ready := s.ready
	s.mu.RUnlock()
	if ready {
		return nil
	}

	if _, err := s.MetaClient.CreateDatabase(s.Config.Database); err != nil {
		return err
	}

	// The service is now ready.
	s.mu.Lock()
	s.ready = true
	s.mu.Unlock()
	return nil
}

// WithLogger sets the service's logger.
func (s *Service) With(d Diagnostic) {
	s.Diagnostic = d
}

// Statistics maintains statistics for the collectd service.
type Statistics struct {
	PointsReceived       int64
	BytesReceived        int64
	PointsParseFail      int64
	ReadFail             int64
	BatchesTransmitted   int64
	PointsTransmitted    int64
	BatchesTransmitFail  int64
	InvalidDroppedPoints int64
}

// Statistics returns statistics for periodic monitoring.
func (s *Service) Statistics(tags map[string]string) []models.Statistic {
	return []models.Statistic{{
		Name: "collectd",
		Tags: s.defaultTags.Merge(tags),
		Values: map[string]interface{}{
			statPointsReceived:       atomic.LoadInt64(&s.stats.PointsReceived),
			statBytesReceived:        atomic.LoadInt64(&s.stats.BytesReceived),
			statPointsParseFail:      atomic.LoadInt64(&s.stats.PointsParseFail),
			statReadFail:             atomic.LoadInt64(&s.stats.ReadFail),
			statBatchesTransmitted:   atomic.LoadInt64(&s.stats.BatchesTransmitted),
			statPointsTransmitted:    atomic.LoadInt64(&s.stats.PointsTransmitted),
			statBatchesTransmitFail:  atomic.LoadInt64(&s.stats.BatchesTransmitFail),
			statDroppedPointsInvalid: atomic.LoadInt64(&s.stats.InvalidDroppedPoints),
		},
	}}
}

// SetTypes sets collectd types db.
func (s *Service) SetTypes(types string) (err error) {
	reader := strings.NewReader(types)
	s.popts.TypesDB, err = api.NewTypesDB(reader)
	return
}

// Addr returns the listener's address. It returns nil if listener is closed.
func (s *Service) Addr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *Service) serve() {
	// From https://collectd.org/wiki/index.php/Binary_protocol
	//   1024 bytes (payload only, not including UDP / IP headers)
	//   In versions 4.0 through 4.7, the receive buffer has a fixed size
	//   of 1024 bytes. When longer packets are received, the trailing data
	//   is simply ignored. Since version 4.8, the buffer size can be
	//   configured. Version 5.0 will increase the default buffer size to
	//   1452 bytes (the maximum payload size when using UDP/IPv6 over
	//   Ethernet).
	buffer := make([]byte, 1452)

	for {
		select {
		case <-s.done:
			// We closed the connection, time to go.
			return
		default:
			// Keep processing.
		}

		n, _, err := s.conn.ReadFromUDP(buffer)
		if err != nil {
			atomic.AddInt64(&s.stats.ReadFail, 1)
			if s.Diagnostic != nil {
				s.Diagnostic.ReadFromUDPError(err)
			}
			continue
		}
		if n > 0 {
			atomic.AddInt64(&s.stats.BytesReceived, int64(n))
			s.handleMessage(buffer[:n])
		}
	}
}

func (s *Service) handleMessage(buffer []byte) {
	valueLists, err := network.Parse(buffer, s.popts)
	if err != nil {
		atomic.AddInt64(&s.stats.PointsParseFail, 1)
		if s.Diagnostic != nil {
			s.Diagnostic.ParseError(err)
		}
		return
	}
	var points []models.Point
	for _, valueList := range valueLists {
		if s.Config.ParseMultiValuePlugin == "join" {
			points = s.UnmarshalValueListPacked(valueList)
		} else {
			points = s.UnmarshalValueList(valueList)
		}
		for _, p := range points {
			s.batcher.In() <- p
		}
		atomic.AddInt64(&s.stats.PointsReceived, int64(len(points)))
	}
}

func (s *Service) writePoints() {
	for {
		select {
		case <-s.done:
			return
		case batch := <-s.batcher.Out():
			// Will attempt to create database if not yet created.
			if err := s.createInternalStorage(); err != nil {
				if s.Diagnostic != nil {
					s.Diagnostic.InternalStorageCreateError(err)
				}
				continue
			}

			if err := s.PointsWriter.WritePointsPrivileged(s.Config.Database, s.Config.RetentionPolicy, models.ConsistencyLevelAny, batch); err == nil {
				atomic.AddInt64(&s.stats.BatchesTransmitted, 1)
				atomic.AddInt64(&s.stats.PointsTransmitted, int64(len(batch)))
			} else {
				if s.Diagnostic != nil {
					s.Diagnostic.PointWriterError(s.Config.Database, err)
				}
				atomic.AddInt64(&s.stats.BatchesTransmitFail, 1)
			}
		}
	}
}

// UnmarshalValueListPacked is an alternative to the original UnmarshalValueList.
// The difference is that the original provided measurements like (PLUGIN_DSNAME, ["value",xxx])
// while this one will provide measurements like (PLUGIN, {["DSNAME",xxx]}).
// This effectively joins collectd data that should go together, such as:
// (df, {["used",1000],["free",2500]}).
func (s *Service) UnmarshalValueListPacked(vl *api.ValueList) []models.Point {
	timestamp := vl.Time.UTC()

	var name = vl.Identifier.Plugin
	tags := make(map[string]string, 4)
	fields := make(map[string]interface{}, len(vl.Values))

	if vl.Identifier.Host != "" {
		tags["host"] = vl.Identifier.Host
	}
	if vl.Identifier.PluginInstance != "" {
		tags["instance"] = vl.Identifier.PluginInstance
	}
	if vl.Identifier.Type != "" {
		tags["type"] = vl.Identifier.Type
	}
	if vl.Identifier.TypeInstance != "" {
		tags["type_instance"] = vl.Identifier.TypeInstance
	}

	for i, v := range vl.Values {
		fieldName := vl.DSName(i)
		switch value := v.(type) {
		case api.Gauge:
			fields[fieldName] = float64(value)
		case api.Derive:
			fields[fieldName] = float64(value)
		case api.Counter:
			fields[fieldName] = float64(value)
		}
	}
	// Drop invalid points
	p, err := models.NewPoint(name, models.NewTags(tags), fields, timestamp)
	if err != nil {
		if s.Diagnostic != nil {
			s.Diagnostic.DroppingPoint(name, err)
		}
		atomic.AddInt64(&s.stats.InvalidDroppedPoints, 1)
		return nil
	}

	return []models.Point{p}
}

// UnmarshalValueList translates a ValueList into InfluxDB data points.
func (s *Service) UnmarshalValueList(vl *api.ValueList) []models.Point {
	timestamp := vl.Time.UTC()

	var points []models.Point
	for i := range vl.Values {
		var name string
		name = fmt.Sprintf("%s_%s", vl.Identifier.Plugin, vl.DSName(i))
		tags := make(map[string]string, 4)
		fields := make(map[string]interface{}, 1)

		// Convert interface back to actual type, then to float64
		switch value := vl.Values[i].(type) {
		case api.Gauge:
			fields["value"] = float64(value)
		case api.Derive:
			fields["value"] = float64(value)
		case api.Counter:
			fields["value"] = float64(value)
		}

		if vl.Identifier.Host != "" {
			tags["host"] = vl.Identifier.Host
		}
		if vl.Identifier.PluginInstance != "" {
			tags["instance"] = vl.Identifier.PluginInstance
		}
		if vl.Identifier.Type != "" {
			tags["type"] = vl.Identifier.Type
		}
		if vl.Identifier.TypeInstance != "" {
			tags["type_instance"] = vl.Identifier.TypeInstance
		}

		// Drop invalid points
		p, err := models.NewPoint(name, models.NewTags(tags), fields, timestamp)
		if err != nil {
			if s.Diagnostic != nil {
				s.Diagnostic.DroppingPoint(name, err)
			}
			atomic.AddInt64(&s.stats.InvalidDroppedPoints, 1)
			continue
		}

		points = append(points, p)
	}
	return points
}
