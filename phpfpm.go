package collector

import (
	"encoding/json"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"gopkg.in/alecthomas/kingpin.v2"
	"io/ioutil"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	fcgiclient "tomasen/fcgi_client"
)

const (
	namespaces = "phpfpm"
)

var (
	phpEndPoint = kingpin.Flag("collector.phpfpm.endpoint", "phpfpm endPonit address.").Default("tcp://localhost:9000/status").String()
)

type timestamp time.Time

type requestDuration int64

type PoolProcess struct {
	PID               int64           `json:"pid"`
	State             string          `json:"state"`
	StartTime         int64           `json:"start time"`
	StartSince        int64           `json:"start since"`
	Requests          int64           `json:"requests"`
	RequestDuration   requestDuration `json:"request duration"`
	RequestMethod     string          `json:"request method"`
	RequestURI        string          `json:"request uri"`
	ContentLength     int64           `json:"content length"`
	User              string          `json:"user"`
	Script            string          `json:"script"`
	LastRequestCPU    float64         `json:"last request cpu"`
	LastRequestMemory int64           `json:"last request memory"`
}

type Pool struct {
	Error error `json:"error"`
	// The address of the pool, e.g. tcp://127.0.0.1:9000 or unix:///tmp/php-fpm.sock
	Address             string        `json:"-"`
	ScrapeError         error         `json:"-"`
	ScrapeFailures      int64         `json:"-"`
	Name                string        `json:"pool"`
	ProcessManager      string        `json:"process_manager"`
	StartTime           int64         `json:"start time"`
	StartSince          int64         `json:"start since"`
	AcceptedConnections int64         `json:"accepted conn"`
	ListenQueue         int64         `json:"listen queue"`
	MaxListenQueue      int64         `json:"max listen queue"`
	ListenQueueLength   int64         `json:"listen queue len"`
	IdleProcesses       int64         `json:"idle processes"`
	ActiveProcesses     int64         `json:"active processes"`
	TotalProcesses      int64         `json:"total processes"`
	MaxActiveProcesses  int64         `json:"max active processes"`
	MaxChildrenReached  int64         `json:"max children reached"`
	SlowRequests        int64         `json:"slow requests"`
	Processes           []PoolProcess `json:"processes"`
}

type phpFpmCollector struct {
	mutex                    sync.Mutex
	PoolManager              PoolManager
	CountProcessState        bool
	up                       *prometheus.Desc
	scrapeFailues            *prometheus.Desc
	startSince               *prometheus.Desc
	acceptedConnections      *prometheus.Desc
	listenQueue              *prometheus.Desc
	maxListenQueue           *prometheus.Desc
	listenQueueLength        *prometheus.Desc
	idleProcesses            *prometheus.Desc
	activeProcesses          *prometheus.Desc
	totalProcesses           *prometheus.Desc
	maxActiveProcesses       *prometheus.Desc
	maxChildrenReached       *prometheus.Desc
	slowRequests             *prometheus.Desc
	processRequests          *prometheus.Desc
	processLastRequestMemory *prometheus.Desc
	processLastRequestCPU    *prometheus.Desc
	processRequestDuration   *prometheus.Desc
	processState             *prometheus.Desc
}

type PoolManager struct {
	Pools []Pool `json:"pools"`
}

func init() {
	registerCollector("phpfpm", defaultDisabled, NewPHPFPMCollector)
}

func newFuncMetric(metricName string, docString string, labels []string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName(namespaces, "", metricName),
		docString, labels, nil,
	)
}

func NewPHPFPMCollector() (Collector, error) {

	return &phpFpmCollector{
		up:                  newFuncMetric("up", "able to contact php-fpm", nil),
		acceptedConnections: newFuncMetric("accepted_connections_total", "Total number of accepted connections.", nil),
		totalProcesses:      newFuncMetric("total_processes", "The number of idle + active processes.", nil),
		startSince:          newFuncMetric("start_since", "The number of seconds since FPM has started.", nil),
		slowRequests:        newFuncMetric("slow_requests", "The number of requests that exceeded your 'request_slowlog_timeout' value.", nil),
		idleProcesses:       newFuncMetric("idle_processes", "The number of idle processes.' value.", nil),
		activeProcesses:     newFuncMetric("active_processes", "The number of active processes.", nil),
		listenQueueLength:   newFuncMetric("listen_queue_length", "The size of the socket queue of pending connections.", nil),
		maxListenQueue:      newFuncMetric("max_listen_queue", "The maximum number of requests in the queue of pending connections since FPM has started.", nil),
		listenQueue:         newFuncMetric("listen_queue", "The number of requests in the queue of pending connections.", nil),
		maxChildrenReached:  newFuncMetric("max_children_reached", "The number of times, the process limit has been reached, when pm tries to start more children (works only for pm 'dynamic' and 'ondemand').", nil),
	}, nil
}

func UpToDate(addr string) (pool Pool, err error) {

	pool.ScrapeError = nil

	scheme, address, path, err := parseURL(addr)
	if err != nil {
		return pool, pool.error(err)
	}

	fcgi, err := fcgiclient.DialTimeout(scheme, address, time.Duration(3)*time.Second)
	if err != nil {
		return pool, pool.error(err)
	}

	defer fcgi.Close()

	env := map[string]string{
		"SCRIPT_FILENAME": path,
		"SCRIPT_NAME":     path,
		"SERVER_SOFTWARE": "go/php-fpm_exporter",
		"REMOTE_ADDR":     "127.0.0.1",
		"QUERY_STRING":    "json&full",
	}

	resp, err := fcgi.Get(env)

	if err != nil {
		return pool, pool.error(err)
	}

	defer resp.Body.Close()

	content, err := ioutil.ReadAll(resp.Body)

	if err != nil {
		return pool, pool.error(err)
	}

	content = JSONResponseFixer(content)

	log.Debugf("Pool[%v]: %v", addr, string(content))

	if err = json.Unmarshal(content, &pool); err != nil {
		log.Errorf("Pool[%v]: %v", addr, err)
		return
	}
	return
}

func parseURL(rawurl string) (scheme string, address string, path string, err error) {
	uri, err := url.Parse(rawurl)
	if err != nil {
		return uri.Scheme, uri.Host, uri.Path, err
	}

	scheme = uri.Scheme

	switch uri.Scheme {
	case "unix":
		result := strings.Split(uri.Path, ";")
		address = result[0]
		if len(result) > 1 {
			path = result[1]
		}
	default:
		address = uri.Host
		path = uri.Path
	}

	return
}

func JSONResponseFixer(content []byte) []byte {
	c := string(content)
	re := regexp.MustCompile(`(,"request uri":)"(.*?)"(,"content length":)`)
	matches := re.FindAllStringSubmatch(c, -1)

	for _, match := range matches {
		requestURI, _ := json.Marshal(match[2])

		sold := match[0]
		snew := match[1] + string(requestURI) + match[3]

		c = strings.Replace(c, sold, snew, -1)
	}

	return []byte(c)
}

func (p *Pool) error(err error) error {
	p.ScrapeError = err
	p.ScrapeFailures++
	log.Error(err)
	return err
}

// Describe exposes the metric description to Prometheus

func (c *phpFpmCollector) Update(ch chan<- prometheus.Metric) (err error) {

	var (
		pool Pool
	)

	pool, err = UpToDate(*phpEndPoint)

	if pool.ScrapeError != nil {
		ch <- prometheus.MustNewConstMetric(
			newFuncMetric("up", "phpfpm up status", nil), prometheus.GaugeValue, 0,
		)
		err = pool.ScrapeError
		return
	} else {
		ch <- prometheus.MustNewConstMetric(
			newFuncMetric("up", "phpfpm up status", nil), prometheus.GaugeValue, 1,
		)
	}

	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("accepted_connections_total", "Total number of accepted connections.", nil), prometheus.CounterValue, float64(pool.AcceptedConnections),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("total_processes", "The number of idle + active processes.", nil), prometheus.CounterValue, float64(pool.TotalProcesses),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("start_since", "The number of seconds since FPM has started.", nil), prometheus.CounterValue, float64(pool.StartSince),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("slow_requests", "The number of requests that exceeded your 'request_slowlog_timeout' value.", nil), prometheus.CounterValue, float64(pool.SlowRequests),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("idle_processes", "The number of idle processes.' value.", nil), prometheus.CounterValue, float64(pool.IdleProcesses),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("active_processes", "The number of active processes.", nil), prometheus.CounterValue, float64(pool.ActiveProcesses),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("listen_queue_length", "The size of the socket queue of pending connections.", nil), prometheus.CounterValue, float64(pool.ListenQueueLength),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("max_listen_queue", "The maximum number of requests in the queue of pending connections since FPM has started.", nil), prometheus.CounterValue, float64(pool.MaxListenQueue),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("listen_queue", "The number of requests in the queue of pending connections.", nil), prometheus.CounterValue, float64(pool.ListenQueue),
	)
	ch <- prometheus.MustNewConstMetric(
		newFuncMetric("max_children_reached", "The number of times, the process limit has been reached, when pm tries to start more children (works only for pm 'dynamic' and 'ondemand').", nil), prometheus.CounterValue, float64(pool.MaxChildrenReached),
	)
	return nil
}
