package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"html/template"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"gopkg.in/yaml.v3"
)

// Config stores the variables for runtime configuration.
type Config struct {
	Debug    bool
	LogSize  int
	HostName string
	Web      struct {
		Host string
		Port int
	}
	Persistence struct {
		DB string
	}
	Alert struct {
		Telegram struct {
			Token string
			Chat  int64
		}
		Gotify struct {
			Token  string
			Server string
		}
		Exec string
		MQTT struct {
			Server   string
			Port     int
			User     string
			Password string
			Topic    string
		}
	}
	Monitor struct {
		MQTT struct {
			Server          string
			Port            int
			User            string
			Password        string
			History         int
			StandardTimeout float64
			Targets         []struct {
				Topic   string
				Name    string
				Timeout int
			}
		}

		Ping struct {
			Interval  int
			Threshold int
			Targets   []struct {
				Name      string
				Address   string
				Interval  int
				Threshold int
			}
		}
		HTTP struct {
			Interval  int
			Timeout   int
			Threshold int
			Targets   []struct {
				Name      string
				Address   string
				Value     string
				Interval  int
				Timeout   int
				Threshold int
			}
		}
		Exec struct {
			Interval  int
			Timeout   int
			Threshold int
			Targets   []struct {
				Name      string
				Command   string
				Interval  int
				Timeout   int
				Threshold int
			}
		}
	}
}

// TimedEntry stores a string with timestamp.
type TimedEntry struct {
	Timestamp time.Time
	Value     string
}

// MQTTTopic stores status information on a MQTT topic.
type MQTTMonitorData struct {
	Name          string
	FirstSeen     time.Time
	LastSeen      time.Time
	LastError     time.Time
	LastPayload   string
	History       []TimedEntry
	AvgTransmit   float64 `json:"-"`
	Timeout       float64 `json:"-"`
	CustomTimeout float64
	Status        int32
	Samples       int64
	SampleDays    dayBuckets
	Alerts        int64
	Deleted       bool
}

type PingMonitorData struct {
	Name           string
	LastOK         time.Time
	LastError      time.Time
	LastErrorStart time.Time
	Status         int32
	TotalOK        int64
	OKDays         dayBuckets
	TotalError     int64
	ErrorDays      dayBuckets
	Errors         int
	Timestamp      time.Time
	Interval       int
	Threshold      int
}

type HTTPMonitorData struct {
	Name           string
	LastOK         time.Time
	LastError      time.Time
	LastErrorStart time.Time
	Value          string
	LastValue      string
	LastErrorValue string
	Status         int32
	TotalOK        int64
	OKDays         dayBuckets
	TotalError     int64
	ErrorDays      dayBuckets
	Errors         int
	Timestamp      time.Time
	Interval       int
	Timeout        int
	Threshold      int
}

type ExecMonitorData struct {
	Name           string
	LastOK         time.Time
	LastError      time.Time
	LastErrorStart time.Time
	Status         int32
	TotalOK        int64
	OKDays         dayBuckets
	TotalError     int64
	ErrorDays      dayBuckets
	Errors         int
	Timestamp      time.Time
	Interval       int
	Timeout        int
	Threshold      int
}

// MonitorData stores the actual status data of the monitoring process.
type MonitorData struct {
	MQTT         map[string]*MQTTMonitorData
	Ping         map[string]*PingMonitorData
	HTTP         map[string]*HTTPMonitorData
	Exec         map[string]*ExecMonitorData
	sync.RWMutex `json:"-"`
}

// Data struct for serving main web page.
type PageData struct {
	MonitorData      *MonitorData
	Timestamp        time.Time
	Uptime           time.Time
	Config           *Config
	LogHistory       *[]TimedEntry
	Fullversion      *string
	StatusOkCount    int
	StatusWarnCount  int
	StatusErrorCount int
}

// Data struct of JSON payload for MQTT alerts.
type MQTTAlertPayload struct {
	SensorType string    `json:"type"`
	SensorName string    `json:"name"`
	Status     string    `json:"status"`
	Since      time.Time `json:"since"`
	Err        string    `json:"error"`
	Msg        string    `json:"message"`
}

// Data struct of JSON payload for api/stats.
type StatsData struct {
	OkCount  int `json:"ok"`
	ErrCount int `json:"error"`
}

var (
	version     string
	build       string
	commit      string
	fullversion string

	config     *Config
	configFile string
	configLock = new(sync.RWMutex)

	logHistory []TimedEntry
	logLock    = new(sync.RWMutex)

	tgbot *tgbotapi.BotAPI

	monitorData = MonitorData{
		MQTT: make(map[string]*MQTTMonitorData),
		Ping: make(map[string]*PingMonitorData),
		HTTP: make(map[string]*HTTPMonitorData),
		Exec: make(map[string]*ExecMonitorData)}

	uptime = time.Now()

	monitorMqttClient mqtt.Client
	alertMqttClient   mqtt.Client

	persistenceStore *sqlitePersistence
	monitorMqttMu    sync.Mutex
	alertMqttMu      sync.Mutex
	telegramMu       sync.Mutex
	callbackMu       sync.Mutex
	mqttCallbacks    sync.WaitGroup
	stopping         atomic.Bool

	//go:embed templates/index.html
	index_template string

	indexTmpl *template.Template
)

const (
	// MAXLOGSIZE defines the maximum lenght of the log history maintained (can be overridden in config)
	MAXLOGSIZE = 1000
	// Default hostname
	HOSTNAME = "janitor"
	// Status flags for monitoring.
	STATUS_OK    = 0
	STATUS_WARN  = 1
	STATUS_ERROR = 2
	// maxHTTPResponseSize limits the amount of data retained from an HTTP monitor.
	maxHTTPResponseSize = 1 << 20
)

func init() {
	var err error
	indexTmpl, err = template.New("w").Funcs(template.FuncMap{
		"relaTime": relaTime,
		"json": func(i interface{}) template.HTML {
			s, _ := json.MarshalIndent(i, "", "\t")
			return template.HTML(s)
		},
		"floatornot": func(f float64) string {
			if math.IsNaN(f) || f == 0 {
				return "..."
			} else {
				return fmt.Sprintf("%.2f", f)
			}
		},
		"id": func(s string) uint64 {
			h := fnv.New64a()
			h.Write([]byte(s))
			return h.Sum64()
		},
		"statusClass": func(s int32) string {
			switch s {
			case STATUS_WARN:
				return "warn"
			case STATUS_ERROR:
				return "err"
			default:
				return "ok"
			}
		},
		"statusLabel": func(s int32) string {
			switch s {
			case STATUS_WARN:
				return "Warning"
			case STATUS_ERROR:
				return "Error"
			default:
				return "OK"
			}
		}}).Parse(index_template)
	if err != nil {
		panic(err)
	}
}

func main() {
	fullversion = fmt.Sprintf("Janitor %s (build date %s, %s)", version, build, commit)
	// load initial config
	if len(os.Args) != 2 {
		fmt.Println(fullversion)
		fmt.Println("Usage: " + os.Args[0] + " <configfile>")
		os.Exit(1)
	}
	configFile = os.Args[1]
	if !loadConfig() {
		os.Exit(1)
	}
	// start monitoring loop
	stopMonitoring := monitoringLoop()

	// launch web server
	log(fmt.Sprintf("Launching web server at %s:%d", getConfig().Web.Host, getConfig().Web.Port))
	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/reload_config", reloadConfig)
	http.HandleFunc("/delete", deleteWebItem)
	http.HandleFunc("/config", configWebItem)
	http.HandleFunc("/api/stats", serveAPIStats)
	http.HandleFunc("/api/data", serveAPIData)
	http.HandleFunc("/api/metrics", serveAPIMetrics)
	addr := fmt.Sprintf("%s:%d", getConfig().Web.Host, getConfig().Web.Port)
	srv := &http.Server{Addr: addr}

	// start server in background
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log("Web server error: " + err.Error())
		}
	}()

	// wait for shutdown signal
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig

	callbackMu.Lock()
	stopping.Store(true)
	callbackMu.Unlock()
	log("Shutting down...")
	stopMonitoring()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		fmt.Printf("Web server shutdown: %s\n", err)
		_ = srv.Close()
	}
	configurationMu.Lock()
	defer configurationMu.Unlock()
	monitorMqttMu.Lock()
	if monitorMqttClient != nil && monitorMqttClient.IsConnected() {
		monitorMqttClient.Disconnect(250)
	}
	monitorMqttMu.Unlock()
	alertMqttMu.Lock()
	if alertMqttClient != nil && alertMqttClient.IsConnected() {
		alertMqttClient.Disconnect(250)
	}
	alertMqttMu.Unlock()
	mqttCallbacks.Wait()
	if err := closePersistence(); err != nil {
		fmt.Printf("Unable to save state during shutdown: %s\n", err)
		os.Exit(1)
	}
}

// Set defaults for configuration values.
func setDefaults(c *Config) {
	if c.HostName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = HOSTNAME
		}
		c.HostName = hostname
	}
	if c.LogSize == 0 {
		c.LogSize = MAXLOGSIZE
	}
	if c.Web.Port == 0 {
		c.Web.Port = 8080
	}
	if c.Monitor.MQTT.History == 0 {
		c.Monitor.MQTT.History = 10
	}
	if c.Monitor.MQTT.Port == 0 {
		c.Monitor.MQTT.Port = 1883
	}
	if c.Monitor.MQTT.StandardTimeout == 0 {
		c.Monitor.MQTT.StandardTimeout = 1.5
	}
	if c.Monitor.Ping.Interval == 0 {
		c.Monitor.Ping.Interval = 60
	}
	if c.Monitor.Ping.Threshold == 0 {
		c.Monitor.Ping.Threshold = 2
	}
	if c.Monitor.HTTP.Interval == 0 {
		c.Monitor.HTTP.Interval = 60
	}
	if c.Monitor.HTTP.Timeout == 0 {
		c.Monitor.HTTP.Timeout = 5000
	}
	if c.Monitor.HTTP.Threshold == 0 {
		c.Monitor.HTTP.Threshold = 2
	}
	if c.Monitor.Exec.Interval == 0 {
		c.Monitor.Exec.Interval = 60
	}
	if c.Monitor.Exec.Timeout == 0 {
		c.Monitor.Exec.Timeout = 5000
	}
	if c.Monitor.Exec.Threshold == 0 {
		c.Monitor.Exec.Threshold = 2
	}
	if c.Alert.MQTT.Port == 0 {
		c.Alert.MQTT.Port = 1883
	}
}

var configurationMu sync.Mutex
var configurationLoaded bool

// Protected by monitorData. In-flight checks must not update a replacement state.
var monitorGeneration uint64

// Protected by monitorData. Monitor silence is measured from this time when it is
// newer than LastSeen, so restoring an old database does not fire false alerts.
var stateEpoch time.Time

// Loads configuration and persistence atomically, then refreshes integrations.
// A failed read, validation, or persistence transaction preserves live state.
func loadConfig() bool {
	configurationMu.Lock()
	defer configurationMu.Unlock()
	if stopping.Load() {
		return false
	}

	// set up initial config for logging and others to work
	if config == nil {
		config = new(Config)
		setDefaults(config)
	}

	// (re)populate config struct from file. A missing file is not an error: start
	// with defaults and no monitoring. Any other read error or a parse error is fatal.
	newconfig := new(Config)
	yamlFile, err := os.ReadFile(configFile)
	if err != nil {
		// A missing file is only tolerated on the initial load, when starting
		// without monitoring is the intended behavior. On reload it must be
		// fatal: treating it as an empty config would delete live monitor state
		// and persist that deletion.
		if !os.IsNotExist(err) || configurationLoaded {
			log("Unable to load config: " + err.Error())
			return false
		}
		log("Config file not found, starting without monitoring: " + configFile)
	} else if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(yamlFile))), newconfig); err != nil {
		log("Unable to load config: " + err.Error())
		return false
	}

	setDefaults(newconfig)

	if err := configurePersistenceForConfig(newconfig, !configurationLoaded); err != nil {
		log("Unable to load config: " + err.Error())
		return false
	}
	configurationLoaded = true
	log("Starting " + fullversion)
	debug("Loaded config: " + fmt.Sprintf("%+v", getConfig()))

	// Reconnect the monitoring and alerting MQTT clients independently so that a
	// slow Telegram or MQTT connection cannot block the other integration.
	monitorMqttMu.Lock()
	if getConfig().Monitor.MQTT.Server == "" {
		if monitorMqttClient != nil {
			if monitorMqttClient.IsConnected() {
				monitorMqttClient.Disconnect(1)
			}
			monitorMqttClient = nil
		}
	} else {
		if monitorMqttClient != nil && monitorMqttClient.IsConnected() {
			monitorMqttClient.Disconnect(1)
			debug("Disconnected from MQTT (monitoring)")
		}
		connectMqtt()
	}
	monitorMqttMu.Unlock()

	// connect Telegram if configured
	if getConfig().Alert.Telegram.Token != "" && getConfig().Alert.Telegram.Chat != 0 {
		connectTelegram()
	}

	alertMqttMu.Lock()
	if getConfig().Alert.MQTT.Server == "" || getConfig().Alert.MQTT.Topic == "" {
		if alertMqttClient != nil {
			if alertMqttClient.IsConnected() {
				alertMqttClient.Disconnect(1)
			}
			alertMqttClient = nil
		}
	} else {
		if alertMqttClient != nil && alertMqttClient.IsConnected() {
			alertMqttClient.Disconnect(1)
			debug("Disconnected from MQTT (alerting)")
		}
		connectMqttAlert()
	}
	alertMqttMu.Unlock()
	return true
}

func connectTelegram() {
	telegramMu.Lock()
	defer telegramMu.Unlock()
	connectTelegramLocked(getConfig())
}

func connectTelegramLocked(c *Config) {
	var err error
	tgbot, err = tgbotapi.NewBotAPIWithClient(c.Alert.Telegram.Token, tgbotapi.APIEndpoint, &http.Client{Timeout: 10 * time.Second})
	if err != nil {
		log("Unable to connect to Telegram: " + err.Error())
	} else {
		log("Connected to telegram bot")
	}
}

func connectMqtt() {
	opts := mqtt.NewClientOptions()
	opts.SetConnectTimeout(5 * time.Second)
	opts.AddBroker(fmt.Sprintf("%s:%d", getConfig().Monitor.MQTT.Server, getConfig().Monitor.MQTT.Port))
	opts.SetUsername(getConfig().Monitor.MQTT.User)
	opts.SetPassword(getConfig().Monitor.MQTT.Password)
	opts.OnConnect = func(c mqtt.Client) {

		topics := make(map[string]byte)
		for _, t := range getConfig().Monitor.MQTT.Targets {
			topics[t.Topic] = byte(0)
		}

		// deduplicate MQTT topics (remove specific topics that are included in wildcard topics)
		for t := range topics {
			if strings.Contains(t, "#") {
				for tt := range topics {
					if matchMQTTTopic(t, tt) && t != tt {
						delete(topics, tt)
						debug(fmt.Sprintf("Deleting %s from MQTT subscription (included in %s)", tt, t))
					}
				}
			}
		}

		t := make([]string, 0)
		for i := range topics {
			t = append(t, i)
		}

		if token := c.SubscribeMultiple(topics, onMessageReceived); token.Wait() && token.Error() != nil {
			log("Unable to subscribe to MQTT: " + token.Error().Error())
		} else {
			log("Subscribed to MQTT topics: " + strings.Join(t, ", "))
		}
	}

	monitorMqttClient = mqtt.NewClient(opts)
	if token := monitorMqttClient.Connect(); token.Wait() && token.Error() != nil {
		log("Unable to connect to MQTT for monitoring: " + token.Error().Error())
	} else {
		log("Connected to MQTT server for monitoring at " + opts.Servers[0].String())
	}
}

func connectMqttAlert() {
	opts := mqtt.NewClientOptions()
	opts.SetConnectTimeout(5 * time.Second)
	opts.AddBroker(fmt.Sprintf("%s:%d", getConfig().Alert.MQTT.Server, getConfig().Alert.MQTT.Port))
	opts.SetUsername(getConfig().Alert.MQTT.User)
	opts.SetPassword(getConfig().Alert.MQTT.Password)
	alertMqttClient = mqtt.NewClient(opts)
	if token := alertMqttClient.Connect(); token.Wait() && token.Error() != nil {
		log("Unable to connect to MQTT for alerting: " + token.Error().Error())
	} else {
		log("Connected to MQTT server for alerting at " + opts.Servers[0].String())
	}
}

// finds a custom name in the configuration for a given topic
func findTopicName(topic string) string {
	return topicName(getConfig(), topic)
}

// Receives an MQTT message and updates status accordingly.
func onMessageReceived(client mqtt.Client, message mqtt.Message) {
	callbackMu.Lock()
	if stopping.Load() {
		callbackMu.Unlock()
		return
	}
	mqttCallbacks.Add(1)
	callbackMu.Unlock()
	defer mqttCallbacks.Done()
	debug("MQTT: " + message.Topic() + ": " + string(message.Payload()))

	monitorData.Lock()
	if !mqttTopicConfigured(getConfig(), message.Topic()) {
		monitorData.Unlock()
		return
	}

	e, ok := monitorData.MQTT[message.Topic()]
	if !ok {
		monitorData.MQTT[message.Topic()] = &MQTTMonitorData{}
		e = monitorData.MQTT[message.Topic()]
		e.Name = findTopicName(message.Topic())
	}

	if e.Deleted {
		monitorData.Unlock()
		return
	}

	e.History = append(e.History, TimedEntry{time.Now(), string(message.Payload())})
	if len(e.History) > getConfig().Monitor.MQTT.History {
		e.History = e.History[len(e.History)-getConfig().Monitor.MQTT.History:]
	}

	if len(e.History) > 1 {
		var total float64 = 0
		for i, v := range e.History {
			if i > 0 {
				total += v.Timestamp.Sub(e.History[i-1].Timestamp).Seconds()
			}
		}
		e.AvgTransmit = total / float64(len(e.History)-1)
	}
	if e.FirstSeen.IsZero() {
		e.FirstSeen = time.Now()
	}
	e.LastSeen = time.Now()
	e.LastPayload = string(message.Payload())
	now := time.Now()
	e.SampleDays = recordBucket(e.SampleDays, now)
	e.Samples = e.SampleDays.total(now)
	monitorData.Unlock()
	persistState()

}

// Launch infinite loops for monitoring and alerting.
var monitoringContext = context.Background()

// Returns a stop function which cancels in-flight checks and waits for every
// monitoring producer to finish before the database is closed.
func monitoringLoop() func() {
	debug("Entering monitoring loop")
	ctx, cancel := context.WithCancel(context.Background())
	monitoringContext = ctx
	var workers sync.WaitGroup
	for _, check := range []func(){evaluateMQTT, checkPing, checkHTTP, checkExec} {
		workers.Add(1)
		go func(check func()) {
			defer workers.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				check()
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}(check)
	}
	return func() { cancel(); workers.Wait() }
}

type pendingAlert struct {
	sensorType string
	sensorName string
	status     int
	since      time.Time
	msg        string
}

// Periodically evaluate MQTT monitoring targets and issue alerts/recoveries as needed.
func evaluateMQTT() {

	monitorMqttMu.Lock()
	if !stopping.Load() && getConfig().Monitor.MQTT.Server != "" {
		if monitorMqttClient == nil || !monitorMqttClient.IsConnected() {
			connectMqtt()
		}
	}
	monitorMqttMu.Unlock()

	evaluateMQTTState()
}

// Evaluate state without initiating a broker connection from a web edit.
func evaluateMQTTState() {
	monitorData.Lock()

	var alerts []pendingAlert
	changed := false

	for topic, v := range monitorData.MQTT {

		if v.Deleted {
			continue
		}

		previousStatus := v.Status
		now := time.Now()
		// Measure silence from the loaded epoch so a restored database does not
		// immediately report every topic as timed out.
		lastSeen := v.LastSeen
		if lastSeen.Before(stateEpoch) {
			lastSeen = stateEpoch
		}
		elapsed := now.Sub(lastSeen).Seconds()

		previousSamples := v.Samples
		v.Samples = v.SampleDays.total(now)
		changed = changed || previousSamples != v.Samples

		// active is true once a message arrived since this state was (re)loaded.
		// Until then, keep the restored status rather than reporting a recovery.
		active := v.LastSeen.After(stateEpoch)

		var timeout float64
		// use overridden timeout if specified
		if v.CustomTimeout > 0 {
			timeout = v.CustomTimeout
		} else {
			// use configured timeout otherwise (general, specific)
			timeout = v.AvgTransmit * getConfig().Monitor.MQTT.StandardTimeout
			for _, t := range getConfig().Monitor.MQTT.Targets {
				if matchMQTTTopic(t.Topic, topic) && t.Timeout > 0 {
					timeout = float64(t.Timeout)
					break
				}
			}
		}

		// Store calculated timeout for showing on web
		v.Timeout = timeout

		// no timeout can be determined yet (single sample or no config) -> skip
		if math.IsNaN(timeout) || timeout == 0 {
			monitorData.MQTT[topic] = v
			continue
		}

		if elapsed > timeout {
			if v.Status != STATUS_ERROR {
				alerts = append(alerts, pendingAlert{"MQTT", v.Name, STATUS_ERROR, lastSeen, fmt.Sprintf("timeout %.2fs", timeout)})
				v.LastError = time.Now()
				v.Alerts++
			}
			v.Status = STATUS_ERROR
		} else if !active {
			// Restored state without new activity: hold the previous status.
		} else if v.AvgTransmit > 0 && elapsed > v.AvgTransmit {
			v.Status = STATUS_WARN
		} else {
			if v.Status == STATUS_ERROR {
				alerts = append(alerts, pendingAlert{"MQTT", v.Name, STATUS_OK, v.LastError, ""})
			}
			v.Status = STATUS_OK
		}
		changed = changed || previousStatus != v.Status
		monitorData.MQTT[topic] = v
	}

	monitorData.Unlock()
	if changed {
		persistState()
	}

	for _, a := range alerts {
		alert(a.sensorType, a.sensorName, a.status, a.since, a.msg)
	}
}

// Matches and MQTT topic 'subject' to a topic pattern 'pattern', potentially containing wildcard ('#' / '+').
// Based on https://github.com/RangerMauve/mqtt-pattern
// Returns true if matches, false if not.
func matchMQTTTopic(pattern string, subject string) bool {
	sl := strings.Split(subject, "/")
	pl := strings.Split(pattern, "/")

	slen := len(sl)
	plen := len(pl)
	lasti := plen - 1

	for i := range pl {

		if pl[i] == "#" {
			return i == lasti
		}
		if i >= slen {
			return false
		}
		if pl[i] == "+" {
			continue
		}
		if strings.ContainsAny(pl[i], "+#") || pl[i] != sl[i] {
			return false
		}
	}
	return plen == slen
}

// Periodically iterate through ping targets and perform check if required.
func checkPing() {
	type pingEntry struct {
		address  string
		interval int
		timeout  int
	}
	monitorData.RLock()
	generation := monitorGeneration
	var toCheck []pingEntry
	for address, e := range monitorData.Ping {
		if e.Timestamp.Add(time.Duration(e.Interval) * time.Second).Before(time.Now()) {
			toCheck = append(toCheck, pingEntry{address, e.Interval, e.Threshold})
		}
	}
	monitorData.RUnlock()

	for _, entry := range toCheck {
		address := entry.address
		r := ping(address)
		if monitoringContext.Err() != nil {
			return
		}
		debug(fmt.Sprintf("Pinging %s: %t", address, r))

		var pending []pendingAlert
		monitorData.Lock()
		e, ok := monitorData.Ping[address]
		ok = ok && generation == monitorGeneration
		if ok {
			now := time.Now()
			e.Timestamp = now
			if r {
				e.OKDays = recordBucket(e.OKDays, now)
				e.LastOK = time.Now()
				e.Errors = 0
				if e.Status == STATUS_ERROR {
					pending = append(pending, pendingAlert{"Ping", e.Name, STATUS_OK, e.LastErrorStart, ""})
				}
				e.Status = STATUS_OK
			} else {
				e.Errors++
				e.ErrorDays = recordBucket(e.ErrorDays, now)
				e.LastError = time.Now()
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				}
				if e.Status == STATUS_WARN && e.Errors >= e.Threshold {
					pending = append(pending, pendingAlert{"Ping", e.Name, STATUS_ERROR, e.LastOK, ""})
					e.Status = STATUS_ERROR
					e.LastErrorStart = time.Now()
				}
			}
			e.TotalOK = e.OKDays.total(now)
			e.TotalError = e.ErrorDays.total(now)
		}
		monitorData.Unlock()
		if ok {
			persistState()
		}
		for _, a := range pending {
			alert(a.sensorType, a.sensorName, a.status, a.since, a.msg)
		}
	}
}

// Periodically iterate through HTTP targets and perform check if required.
func checkHTTP() {
	type httpEntry struct {
		address string
		value   string
		timeout int
	}
	monitorData.RLock()
	generation := monitorGeneration
	var toCheck []httpEntry
	for address, e := range monitorData.HTTP {
		if e.Timestamp.Add(time.Duration(e.Interval) * time.Second).Before(time.Now()) {
			toCheck = append(toCheck, httpEntry{address, e.Value, e.Timeout})
		}
	}
	monitorData.RUnlock()

	for _, entry := range toCheck {
		address := entry.address
		r, errStr, val := performHTTPCheck(address, entry.value, entry.timeout)
		if monitoringContext.Err() != nil {
			return
		}
		debug(fmt.Sprintf("HTTP request %s: %t %s", address, r, errStr))

		var pending []pendingAlert
		monitorData.Lock()
		e, ok := monitorData.HTTP[address]
		ok = ok && generation == monitorGeneration
		if ok {
			now := time.Now()
			e.Timestamp = now
			if r {
				e.OKDays = recordBucket(e.OKDays, now)
				e.LastOK = time.Now()
				e.LastValue = val
				e.Errors = 0
				if e.Status == STATUS_ERROR {
					pending = append(pending, pendingAlert{"HTTP", e.Name, STATUS_OK, e.LastErrorStart, ""})
				}
				e.Status = STATUS_OK
			} else {
				e.Errors++
				e.ErrorDays = recordBucket(e.ErrorDays, now)
				e.LastError = time.Now()
				e.LastErrorValue = errStr
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				}
				if e.Status == STATUS_WARN && e.Errors >= e.Threshold {
					pending = append(pending, pendingAlert{"HTTP", e.Name, STATUS_ERROR, e.LastOK, errStr})
					e.Status = STATUS_ERROR
					e.LastErrorStart = time.Now()
				}
			}
			e.TotalOK = e.OKDays.total(now)
			e.TotalError = e.ErrorDays.total(now)
		}
		monitorData.Unlock()
		if ok {
			persistState()
		}
		for _, a := range pending {
			alert(a.sensorType, a.sensorName, a.status, a.since, a.msg)
		}
	}
}

// Periodically iterate through exec targets and perform check if required.
func checkExec() {
	type execEntry struct {
		command string
		timeout int
	}
	monitorData.RLock()
	generation := monitorGeneration
	var toCheck []execEntry
	for command, e := range monitorData.Exec {
		if e.Timestamp.Add(time.Duration(e.Interval) * time.Second).Before(time.Now()) {
			toCheck = append(toCheck, execEntry{command, e.Timeout})
		}
	}
	monitorData.RUnlock()

	for _, entry := range toCheck {
		command := entry.command
		r := performExecCheck(command, entry.timeout)
		if monitoringContext.Err() != nil {
			return
		}

		var pending []pendingAlert
		monitorData.Lock()
		e, ok := monitorData.Exec[command]
		ok = ok && generation == monitorGeneration
		if ok {
			now := time.Now()
			e.Timestamp = now
			if r {
				e.OKDays = recordBucket(e.OKDays, now)
				e.LastOK = time.Now()
				e.Errors = 0
				if e.Status == STATUS_ERROR {
					pending = append(pending, pendingAlert{"Exec", e.Name, STATUS_OK, e.LastErrorStart, ""})
				}
				e.Status = STATUS_OK
			} else {
				e.Errors++
				e.ErrorDays = recordBucket(e.ErrorDays, now)
				e.LastError = time.Now()
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				}
				if e.Status == STATUS_WARN && e.Errors >= e.Threshold {
					pending = append(pending, pendingAlert{"Exec", e.Name, STATUS_ERROR, e.LastOK, ""})
					e.Status = STATUS_ERROR
					e.LastErrorStart = time.Now()
				}
			}
			e.TotalOK = e.OKDays.total(now)
			e.TotalError = e.ErrorDays.total(now)
		}
		monitorData.Unlock()
		if ok {
			persistState()
		}
		for _, a := range pending {
			alert(a.sensorType, a.sensorName, a.status, a.since, a.msg)
		}
	}
}

// Perform exec check for a single target.
// Return false in case of error or timeout.
func performExecCheck(command string, timeout int) bool {
	ctx, cancel := context.WithTimeout(monitoringContext, time.Duration(timeout)*time.Millisecond)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
	// A descendant may inherit stdout even after the shell is killed.
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	debug(fmt.Sprintf("Exec %s output: %s", command, out))
	if ctx.Err() == context.DeadlineExceeded {
		debug(fmt.Sprintf("Exec %s: timeout exceeded", command))
		return false
	} else if err != nil {
		debug(fmt.Sprintf("Exec %s: %s", command, err))
		return false
	} else {
		debug(fmt.Sprintf("Exec %s: OK", command))
		return true
	}
}

// Perform check for a single 'url', potentially matching content against 'pattern'.
// Returns boolean, error string, content string.
func performHTTPCheck(url string, pattern string, timeout int) (bool, string, string) {
	errValue := ""
	okValue := ""

	client := http.Client{
		Timeout: time.Millisecond * time.Duration(timeout),
	}
	req, err := http.NewRequestWithContext(monitoringContext, http.MethodGet, url, nil)
	if err != nil {
		return false, err.Error(), ""
	}
	resp, err := client.Do(req)
	if err != nil {
		errValue = err.Error()
	} else {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			errValue = fmt.Sprintf("status code %d", resp.StatusCode)
		} else {
			bodyBytes, bodyErr := io.ReadAll(io.LimitReader(resp.Body, maxHTTPResponseSize+1))
			if bodyErr != nil {
				errValue = bodyErr.Error()
			} else if len(bodyBytes) > maxHTTPResponseSize {
				errValue = fmt.Sprintf("response body exceeds %d bytes", maxHTTPResponseSize)
			} else {
				okValue = string(bodyBytes)
				if pattern != "" && !strings.Contains(okValue, pattern) {
					errValue = "Response does not match pattern"
				}
			}
		}
	}

	return errValue == "", errValue, okValue
}

// Pings a host with a single packet (Linux and Windows).
// Returns false if ping exits with error code or with "Destination host unreachable".
// Returns true in case of successful ping.
func ping(host string) bool {
	ctx, cancel := context.WithTimeout(monitoringContext, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ping", "-c", "1", host)
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, "ping", "-n", "1", host)
	}
	// A descendant may inherit stdout even after the shell is killed.
	cmd.WaitDelay = 250 * time.Millisecond
	out, err := cmd.Output()
	if err != nil || strings.Contains(string(out), "Destination host unreachable") {
		return false
	}
	return true
}

// Processes a log entry, prepending it to logHistory, truncating logHistory if needed.
func log(s string) {
	logLock.Lock()
	entry := TimedEntry{time.Now(), s}
	fmt.Printf("[%s] %s\n", entry.Timestamp.Format("2006-01-02 15:04:05"), entry.Value)
	logHistory = append(logHistory, TimedEntry{})
	copy(logHistory[1:], logHistory)
	logHistory[0] = entry

	if len(logHistory) > getConfig().LogSize {
		logHistory = logHistory[:getConfig().LogSize]
	}
	logLock.Unlock()
	persistState()
}

// Omits a debug log entry, if debug logging is enabled.
func debug(s string) {
	if getConfig().Debug {
		log("(" + s + ")")
	}
}

// getConfig returns the current configuration.
func getConfig() *Config {
	configLock.RLock()
	defer configLock.RUnlock()
	return config
}

// Serves the main web page.
func serveIndex(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)

	monitorData.RLock()
	defer monitorData.RUnlock()
	logLock.RLock()
	defer logLock.RUnlock()

	var okCount, warnCount, errCount int
	for _, v := range monitorData.MQTT {
		if !v.Deleted {
			switch v.Status {
			case STATUS_OK:
				okCount++
			case STATUS_WARN:
				warnCount++
			case STATUS_ERROR:
				errCount++
			}
		}
	}
	for _, v := range monitorData.Ping {
		switch v.Status {
		case STATUS_OK:
			okCount++
		case STATUS_WARN:
			warnCount++
		case STATUS_ERROR:
			errCount++
		}
	}
	for _, v := range monitorData.HTTP {
		switch v.Status {
		case STATUS_OK:
			okCount++
		case STATUS_WARN:
			warnCount++
		case STATUS_ERROR:
			errCount++
		}
	}
	for _, v := range monitorData.Exec {
		switch v.Status {
		case STATUS_OK:
			okCount++
		case STATUS_WARN:
			warnCount++
		case STATUS_ERROR:
			errCount++
		}
	}

	indexTmpl.Execute(w,
		PageData{
			&monitorData,
			time.Now(),
			uptime,
			getConfig(),
			&logHistory,
			&fullversion,
			okCount,
			warnCount,
			errCount})
}

// Counts targets per type and status
func calcStats() (map[string]int, map[string]int) {
	up := make(map[string]int)
	down := make(map[string]int)

	monitorData.RLock()

	up["mqtt"] = 0
	down["mqtt"] = 0
	for k := range monitorData.MQTT {
		if !monitorData.MQTT[k].Deleted {
			if monitorData.MQTT[k].Status == STATUS_ERROR {
				down["mqtt"]++
			} else {
				up["mqtt"]++
			}
		}
	}
	up["ping"] = 0
	down["ping"] = 0
	for k := range monitorData.Ping {
		if monitorData.Ping[k].Status == STATUS_ERROR {
			down["ping"]++
		} else {
			up["ping"]++
		}
	}
	up["http"] = 0
	down["http"] = 0
	for k := range monitorData.HTTP {
		if monitorData.HTTP[k].Status == STATUS_ERROR {
			down["http"]++
		} else {
			up["http"]++
		}
	}
	up["exec"] = 0
	down["exec"] = 0
	for k := range monitorData.Exec {
		if monitorData.Exec[k].Status == STATUS_ERROR {
			down["exec"]++
		} else {
			up["exec"]++
		}
	}

	monitorData.RUnlock()
	return up, down

}

// Serves the api/stats page.
func serveAPIStats(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)
	o := 0
	e := 0
	up, down := calcStats()
	for k, c := range up {
		o += c
		e += down[k]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsData{o, e})
}

// Serves the api/metrics page.
func serveAPIMetrics(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)
	w.Header().Set("Content-Type", "text/plain")
	fmt.Fprintln(w, "# HELP janitor_targets Number of Janitor targets")
	fmt.Fprintln(w, "# TYPE janitor_targets gauge")
	hostname := prometheusLabelValue(getConfig().HostName)
	up, down := calcStats()
	for t, c := range up {
		fmt.Fprintf(w, "janitor_targets{state=\"%s\", type=\"%s\", host=\"%s\"} %d\n", "up", t, hostname, c)
	}
	for t, c := range down {
		fmt.Fprintf(w, "janitor_targets{state=\"%s\", type=\"%s\", host=\"%s\"} %d\n", "down", t, hostname, c)
	}
}

// prometheusLabelValue escapes a value for use inside a Prometheus label.
func prometheusLabelValue(value string) string {
	return strings.NewReplacer(`\\`, `\\\\`, `"`, `\"`, "\n", `\n`).Replace(value)
}

// Serves the api/data page.
func serveAPIData(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)
	monitorData.RLock()
	defer monitorData.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(&monitorData)
}

// Reloads the config based on web request.
func reloadConfig(w http.ResponseWriter, r *http.Request) {
	log("Reloading config based on web request from " + r.RemoteAddr)
	if !loadConfig() {
		http.Error(w, "Failed to reload config: check the application log for details", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Published an alert message via the methods configured.
func alert(sensorType string, sensorName string, status int, since time.Time, msg string) {

	// construct and post text alert
	var s string

	switch status {
	case STATUS_OK:
		s = fmt.Sprintf("✓ %s OK for %s, in error since %s ago", sensorType, sensorName, relaTime(since))
	case STATUS_ERROR:
		s = fmt.Sprintf("⚠ %s ERROR for %s, last seen %s ago", sensorType, sensorName, relaTime(since))
		if msg != "" {
			s = s + fmt.Sprintf(" (%s)", msg)
		}
	}

	log(s)

	c := getConfig()
	if c.Alert.Telegram.Token != "" && c.Alert.Telegram.Chat != 0 {
		telegramMu.Lock()
		if tgbot == nil {
			connectTelegramLocked(c)
		}
		if tgbot != nil {
			if _, err := tgbot.Send(tgbotapi.NewMessage(c.Alert.Telegram.Chat, s)); err != nil {
				log("Error sending to telegram: " + err.Error())
			}
		}
		telegramMu.Unlock()
	}
	if c.Alert.Gotify.Token != "" && c.Alert.Gotify.Server != "" {
		form := url.Values{"message": {s}, "title": {"Janitor alert"}}
		req, err := http.NewRequestWithContext(monitoringContext, http.MethodPost, c.Alert.Gotify.Server+"/message?token="+c.Alert.Gotify.Token, strings.NewReader(form.Encode()))
		if err == nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			var resp *http.Response
			resp, err = (&http.Client{Timeout: 10 * time.Second}).Do(req)
			if resp != nil {
				resp.Body.Close()
			}
		}
		if err != nil {
			log("Error in Gotify request: " + err.Error())
		}
	}
	if c.Alert.Exec != "" {
		cmd := exec.CommandContext(monitoringContext, "sh", "-c", c.Alert.Exec)
		if runtime.GOOS == "windows" {
			cmd = exec.CommandContext(monitoringContext, c.Alert.Exec)
		}
		cmd.Stdin = strings.NewReader(s)
		if err := cmd.Run(); err != nil {
			log("Error in executing " + c.Alert.Exec + ": " + err.Error())
		}
	}

	// construct and post json payload for MQTT target
	alertMqttMu.Lock()
	defer alertMqttMu.Unlock()
	if getConfig().Alert.MQTT.Server != "" && getConfig().Alert.MQTT.Topic != "" {
		if alertMqttClient == nil || !alertMqttClient.IsConnected() {
			connectMqttAlert()
		}
		if alertMqttClient != nil {
			payload := MQTTAlertPayload{sensorType, sensorName, "", since, msg, s}
			switch status {
			case STATUS_OK:
				payload.Status = "OK"
			case STATUS_ERROR:
				payload.Status = "ERROR"
			}
			b, err := json.Marshal(payload)
			if err != nil {
				log("Unable to compile payload for MQTT alert: " + err.Error())
				return
			}
			if token := alertMqttClient.Publish(getConfig().Alert.MQTT.Topic, 0, false, b); !token.WaitTimeout(5 * time.Second) {
				log("Timeout publishing MQTT alert")
			} else if token.Error() != nil {
				log("Unable to publish MQTT alert: " + token.Error().Error())
			}
		}
	}
}

// Returns human-readable representation of the time duration between 't' and now.
func relaTime(t time.Time) string {
	if t.IsZero() {
		return "inf"
	}
	d := time.Since(t).Round(time.Second)

	day := time.Minute * 60 * 24
	s := ""
	if d > day {
		days := d / day
		d = d - days*day
		s = fmt.Sprintf("%dd", days)
	}

	if d < time.Second {
		return s + d.String()
	} else if m := d % time.Second; m+m < time.Second {
		return s + (d - m).String()
	} else {
		return s + (d + time.Second - m).String()
	}
}
