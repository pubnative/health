package influxdb

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/influxdata/influxdb/client/v2"
	"github.com/pubnative/health"
)

const bufferSize = 10000
const ticker = 1 * time.Second

var chanFullErr = errors.New("InfluxDB channel full, dropping point")

type InfluxDBSink struct {
	db        string
	dbhost    string
	hostname  string
	precision string
	notifier  Notifier
	client    client.Client
	In        chan *point
}

type Notifier interface {
	Notify(err error)
}

type pointsBatch map[string]*point
type point struct {
	name   string
	tags   map[string]string
	fields map[string]interface{}
	time   time.Time
}

func (p *point) makePoint() (*client.Point, error) {
	return client.NewPoint(p.name, p.tags, p.fields, p.time)
}

func (s InfluxDBSink) EmitEvent(job string, event string, kvs map[string]string) {
	s.emitPoint(job, kvs, map[string]interface{}{event: 1})
}
func (s InfluxDBSink) EmitEventErr(job string, event string, err error, kvs map[string]string) {
	s.emitPoint(job, kvs, map[string]interface{}{event: 1, "error": 1})
}
func (s InfluxDBSink) EmitTiming(job string, event string, nanoseconds int64, kvs map[string]string) {
	if kvs == nil {
		kvs = make(map[string]string)
	}
	kvs["event"] = event
	timing := map[string]interface{}{"timing": nanoseconds, event: 1}
	s.emitPoint(job, kvs, timing)
}
func (s InfluxDBSink) EmitGauge(job string, event string, value float64, kvs map[string]string) {
	if kvs == nil {
		kvs = make(map[string]string)
	}
	kvs["event"] = event
	gauge := map[string]interface{}{"gauge": value, event: 1}
	s.emitPoint(job, kvs, gauge)
}
func (s InfluxDBSink) EmitComplete(job string, status health.CompletionStatus, nanoseconds int64, kvs map[string]string) {
	if kvs == nil {
		kvs = make(map[string]string)
	}
	statusStr := status.String()
	kvs["status"] = statusStr
	timing := map[string]interface{}{"timing": nanoseconds, statusStr: 1}
	s.emitPoint(job, kvs, timing)
}

func (s *InfluxDBSink) emitPoint(
	name string,
	tags map[string]string,
	fields map[string]interface{},
) {
	if s.client == nil {
		return
	}
	if tags == nil {
		tags = make(map[string]string)
	}
	tags["hostname"] = s.hostname
	select {
	case s.In <- &point{name, tags, fields, time.Now()}:
	default:
		if s.notifier != nil {
			s.notifier.Notify(chanFullErr)
		}
	}

}

func (s *InfluxDBSink) notifyRecover(r interface{}) {
	if err, ok := r.(error); ok {
		s.notifier.Notify(err)
	} else {
		s.notifier.Notify(errors.New(fmt.Sprintf("%+v", r)))
	}
}

func (s *InfluxDBSink) sendBatch(batch *client.BatchPoints) {
	defer func() {
		if r := recover(); r != nil {
			s.notifyRecover(r)
		}
	}()

	if s.client == nil || len((*batch).Points()) == 0 {
		return
	}
	if _, _, err := s.client.Ping(time.Second); err != nil {
		s.connect()
	}
	if err := s.client.Write(*batch); err != nil {
		if s.notifier != nil {
			s.notifier.Notify(err)
		}
	}
}

func (s *InfluxDBSink) connect() {
	var conn client.Client
	var err error
	if strings.HasPrefix(s.dbhost, "http") {
		conn, err = client.NewHTTPClient(client.HTTPConfig{Addr: s.dbhost})
	} else {
		conn, err = client.NewUDPClient(client.UDPConfig{Addr: s.dbhost})
		go func() {
			<-time.After(1 * time.Minute)
			s.connect()
		}()
	}
	if err != nil && s.notifier != nil {
		s.notifier.Notify(err)
	}
	if err == nil {
		s.client = conn
	}
}

func (s *InfluxDBSink) spawnWorker() {
	w := worker{
		sink:     s,
		swTick:   time.Tick(ticker),
		aggBatch: make(pointsBatch),
	}
	go w.process()
}

type worker struct {
	sink     *InfluxDBSink
	swTick   <-chan time.Time
	aggBatch pointsBatch
}

func (w *worker) process() {
	defer func() {
		if r := recover(); r != nil {
			w.sink.notifyRecover(r)
		}
		go w.process()
	}()

	for {
		select {
		case <-w.swTick:
			w.sendBatch()
		case point := <-w.sink.In:
			increment(w.aggBatch, point)
		}
	}
}

func increment(batch pointsBatch, point *point) {
	key := tagsToKey(point.name, point.tags)
	if existingPoint, ok := batch[key]; ok {
		for fk, v := range point.fields {
			if existingValue, ok := existingPoint.fields[fk]; ok {
				if fk != "gauge" && fk != "timing" {
					existingPoint.fields[fk] = existingValue.(int) + v.(int)
				}
			} else {
				existingPoint.fields[fk] = v
			}
		}
	} else {
		batch[key] = point
	}
}

func tagsToKey(name string, tags map[string]string) string {
	ks := make([]string, len(tags))
	for k := range tags {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var buff bytes.Buffer
	buff.WriteString(name)
	for _, k := range ks {
		buff.WriteString(",")
		buff.WriteString(k)
		buff.WriteString("=")
		buff.WriteString(tags[k])
	}
	return buff.String()
}

func (w *worker) sendBatch() {
	toSend := w.aggBatch
	w.aggBatch = make(pointsBatch)
	b, _ := client.NewBatchPoints(client.BatchPointsConfig{
		Database:  w.sink.db,
		Precision: w.sink.precision,
	})
	for k := range toSend {
		point, err := toSend[k].makePoint()
		if err == nil {
			b.AddPoint(point)
		} else {
			w.sink.notifier.Notify(err)
		}
	}
	go w.sink.sendBatch(&b)
}

func SetupInfluxDBSink(db, dbHost, hostname, precision string, notifier Notifier, workers int) *InfluxDBSink {
	if dbHost == "" {
		return &InfluxDBSink{}
	}

	s := InfluxDBSink{
		hostname:  hostname,
		dbhost:    dbHost,
		db:        db, // note: if using UDP the database is configured by the UDP service
		precision: precision,
		notifier:  notifier,
		In:        make(chan *point, bufferSize),
	}

	s.connect()

	for i := 0; i < workers; i++ {
		s.spawnWorker()
	}

	return &s
}
