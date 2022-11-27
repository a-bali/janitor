package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io/ioutil"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api"
	"gopkg.in/yaml.v2"
)

// Config stores the variables for runtime configuration.
type Config struct {
	Debug   bool
	LogSize int
	Web     struct {
		Host string
		Port int
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
	Alerts        int64
	Deleted       bool
}

type PingMonitorData struct {
	Name       string
	LastOK     time.Time
	LastError  time.Time
	Status     int32
	TotalOK    int64
	TotalError int64
	Errors     int
	Timestamp  time.Time
	Interval   int
	Threshold  int
}

type HTTPMonitorData struct {
	Name           string
	LastOK         time.Time
	LastError      time.Time
	Value          string
	LastValue      string
	LastErrorValue string
	Status         int32
	TotalOK        int64
	TotalError     int64
	Errors         int
	Timestamp      time.Time
	Interval       int
	Timeout        int
	Threshold      int
}

type ExecMonitorData struct {
	Name       string
	LastOK     time.Time
	LastError  time.Time
	Status     int32
	TotalOK    int64
	TotalError int64
	Errors     int
	Timestamp  time.Time
	Interval   int
	Timeout    int
	Threshold  int
}

// MonitorData stores the actual status data of the monitoring process.
type MonitorData struct {
	MQTT map[string]*MQTTMonitorData
	Ping map[string]*PingMonitorData
	HTTP map[string]*HTTPMonitorData
	Exec map[string]*ExecMonitorData
	sync.RWMutex
}

// Data struct for serving main web page.
type PageData struct {
	MonitorData *MonitorData
	Timestamp   time.Time
	Uptime      time.Time
	Config      *Config
	LogHistory  *[]TimedEntry
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

	//go:embed templates/index.html
	index_template string
)

const (
	// MAXLOGSIZE defines the maximum lenght of the log history maintained (can be overridden in config)
	MAXLOGSIZE = 1000
	// Status flags for monitoring.
	STATUS_OK    = 0
	STATUS_WARN  = 1
	STATUS_ERROR = 2
)

func main() {
	// load initial config
	if len(os.Args) != 2 {
		fmt.Println("Usage: " + os.Args[0] + " <configfile>")
		os.Exit(1)
	}
	configFile = os.Args[1]
	loadConfig()
	// start monitoring loop
	monitoringLoop()
	// launch web server
	log(fmt.Sprintf("Launching web server at %s:%d", getConfig().Web.Host, getConfig().Web.Port))
	http.HandleFunc("/", serveIndex)
	http.HandleFunc("/reload_config", reloadConfig)
	http.HandleFunc("/delete", deleteWebItem)
	http.HandleFunc("/config", configWebItem)
	http.HandleFunc("/api/stats", serveAPIStats)
	http.HandleFunc("/api/data", serveAPIData)
	panic(http.ListenAndServe(fmt.Sprintf("%s:%d", getConfig().Web.Host, getConfig().Web.Port), nil))
}

// Set defaults for configuration values.
func setDefaults(c *Config) {
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

// Loads or reloads the configuration and initializes MQTT and Telegram connections accordingly.
func loadConfig() {

	// set up initial config for logging and others to work
	if config == nil {
		config = new(Config)
		setDefaults(config)
	}

	// (re)populate config struct from file
	yamlFile, err := ioutil.ReadFile(configFile)
	if err != nil {
		log("Unable to load config: " + err.Error())
		return
	}

	newconfig := new(Config)

	err = yaml.Unmarshal(yamlFile, &newconfig)
	if err != nil {
		log("Unable to load config: " + err.Error())
	}

	setDefaults(newconfig)

	configLock.Lock()
	config = newconfig
	configLock.Unlock()

	debug("Loaded config: " + fmt.Sprintf("%+v", getConfig()))

	// update monitor targets based on new configuration
	monitorData.Lock()

	// remove deleted MQTT targets
	for k := range monitorData.MQTT {
		if monitorData.MQTT[k].Deleted {
			delete(monitorData.MQTT, k)
		}
	}

	// update monitored ping hosts
	for _, target := range getConfig().Monitor.Ping.Targets {
		e, ok := monitorData.Ping[target.Address]
		if !ok {
			monitorData.Ping[target.Address] = &PingMonitorData{}
			e = monitorData.Ping[target.Address]
		}
		e.Name = target.Name
		if target.Interval != 0 {
			e.Interval = target.Interval
		} else if e.Interval == 0 {
			e.Interval = getConfig().Monitor.Ping.Interval
		}
		if target.Threshold != 0 {
			e.Threshold = target.Threshold
		} else if e.Threshold == 0 {
			e.Threshold = getConfig().Monitor.Ping.Threshold
		}
		monitorData.Ping[target.Address] = e
	}

	// remove deleted ping hosts from monitoring
	for k := range monitorData.Ping {
		found := false
		for _, c := range getConfig().Monitor.Ping.Targets {
			if k == c.Address {
				found = true
				break
			}
		}
		if !found {
			delete(monitorData.Ping, k)
		}
	}

	// update monitored http hosts
	for _, target := range getConfig().Monitor.HTTP.Targets {

		e, ok := monitorData.HTTP[target.Address]
		if !ok {
			monitorData.HTTP[target.Address] = &HTTPMonitorData{}
			e = monitorData.HTTP[target.Address]
		}
		e.Name = target.Name
		if target.Interval != 0 {
			e.Interval = target.Interval
		} else if e.Interval == 0 {
			e.Interval = getConfig().Monitor.HTTP.Interval
		}
		if target.Timeout != 0 {
			e.Timeout = target.Timeout
		} else if e.Timeout == 0 {
			e.Timeout = getConfig().Monitor.HTTP.Timeout
		}
		if target.Threshold != 0 {
			e.Threshold = target.Threshold
		} else if e.Threshold == 0 {
			e.Threshold = getConfig().Monitor.HTTP.Threshold
		}
		e.Value = target.Value
		monitorData.HTTP[target.Address] = e
	}

	// remove deleted http hosts from monitoring
	for k := range monitorData.HTTP {
		found := false
		for _, c := range getConfig().Monitor.HTTP.Targets {
			if k == c.Address {
				found = true
				break
			}
		}
		if !found {
			delete(monitorData.HTTP, k)
		}
	}

	// update monitored exec targets
	for _, target := range getConfig().Monitor.Exec.Targets {

		e, ok := monitorData.Exec[target.Command]
		if !ok {
			monitorData.Exec[target.Command] = &ExecMonitorData{}
			e = monitorData.Exec[target.Command]
		}
		e.Name = target.Name
		if target.Interval != 0 {
			e.Interval = target.Interval
		} else if e.Interval == 0 {
			e.Interval = getConfig().Monitor.Exec.Interval
		}
		if target.Timeout != 0 {
			e.Timeout = target.Timeout
		} else if e.Timeout == 0 {
			e.Timeout = getConfig().Monitor.Exec.Timeout
		}
		if target.Threshold != 0 {
			e.Threshold = target.Threshold
		} else if e.Threshold == 0 {
			e.Threshold = getConfig().Monitor.Exec.Threshold
		}
		monitorData.Exec[target.Command] = e
	}

	// remove deleted exec targets from monitoring
	for k := range monitorData.Exec {
		found := false
		for _, c := range getConfig().Monitor.Exec.Targets {
			if k == c.Command {
				found = true
				break
			}
		}
		if !found {
			delete(monitorData.Exec, k)
		}
	}
	monitorData.Unlock()

	// connect MQTT if configured
	if getConfig().Monitor.MQTT.Server != "" {
		if monitorMqttClient != nil && monitorMqttClient.IsConnected() {
			monitorMqttClient.Disconnect(1)
			debug("Disconnected from MQTT (monitoring)")
		}
		opts := mqtt.NewClientOptions()
		opts.AddBroker(fmt.Sprintf("%s://%s:%d", "tcp", getConfig().Monitor.MQTT.Server, getConfig().Monitor.MQTT.Port))
		opts.SetUsername(getConfig().Monitor.MQTT.User)
		opts.SetPassword(getConfig().Monitor.MQTT.Password)
		opts.OnConnect = func(c mqtt.Client) {

			topics := make(map[string]byte)
			for _, t := range getConfig().Monitor.MQTT.Targets {
				topics[t.Topic] = byte(0)
			}

			// deduplicate MQTT topics (remove specific topics that are included in wildcard topics)
			for t, _ := range topics {
				if strings.Contains(t, "#") {
					for tt, _ := range topics {
						if matchMQTTTopic(t, tt) && t != tt {
							delete(topics, tt)
							debug(fmt.Sprintf("Deleting %s from MQTT subscription (included in %s)", tt, t))
						}
					}
				}
			}

			t := make([]string, 0)
			for i, _ := range topics {
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

	// connect Telegram if configured
	if getConfig().Alert.Telegram.Token != "" && getConfig().Alert.Telegram.Chat != 0 {
		tgbot, err = tgbotapi.NewBotAPI(getConfig().Alert.Telegram.Token)
		if err != nil {
			log("Unable to connect to Telegram: " + err.Error())
		}
		log("Connected to telegram bot")
	}

	// connect MQTT alert topic if configured
	if getConfig().Alert.MQTT.Server != "" && getConfig().Alert.MQTT.Topic != "" {
		if alertMqttClient != nil && alertMqttClient.IsConnected() {
			alertMqttClient.Disconnect(1)
			debug("Disconnected from MQTT (alerting)")
		}
		opts := mqtt.NewClientOptions()
		opts.AddBroker(fmt.Sprintf("%s://%s:%d", "tcp", getConfig().Alert.MQTT.Server, getConfig().Alert.MQTT.Port))
		opts.SetUsername(getConfig().Monitor.MQTT.User)
		opts.SetPassword(getConfig().Monitor.MQTT.Password)
		alertMqttClient = mqtt.NewClient(opts)
		if token := alertMqttClient.Connect(); token.Wait() && token.Error() != nil {
			log("Unable to connect to MQTT for alerting: " + token.Error().Error())
		} else {
			log("Connected to MQTT server for alerting at " + opts.Servers[0].String())
		}

	}
}

// Receives an MQTT message and updates status accordingly.
func onMessageReceived(client mqtt.Client, message mqtt.Message) {
	debug("MQTT: " + message.Topic() + ": " + string(message.Payload()))

	monitorData.Lock()
	defer monitorData.Unlock()

	e, ok := monitorData.MQTT[message.Topic()]
	if !ok {
		monitorData.MQTT[message.Topic()] = &MQTTMonitorData{}
		e = monitorData.MQTT[message.Topic()]
	}

	if e.Deleted {
		return
	}

	e.History = append(e.History, TimedEntry{time.Now(), string(message.Payload())})
	if len(e.History) > getConfig().Monitor.MQTT.History {
		e.History = e.History[1:]
	}

	var total float64 = 0
	for i, v := range e.History {
		if i > 0 {
			total += v.Timestamp.Sub(e.History[i-1].Timestamp).Seconds()
		}
	}
	e.AvgTransmit = total / float64(len(e.History)-1)
	if e.FirstSeen.IsZero() {
		e.FirstSeen = time.Now()
	}
	e.LastSeen = time.Now()
	e.LastPayload = string(message.Payload())
	e.Samples++

	//monitorData.MQTT[message.Topic()] = e

}

// Launch infinite loops for monitoring and alerting.
func monitoringLoop() {
	debug("Entering monitoring loop")
	go func() {
		for {
			evaluateMQTT()
			time.Sleep(time.Second)
		}
	}()

	go func() {
		for {
			checkPing()
			time.Sleep(time.Second)
		}
	}()

	go func() {
		for {
			checkHTTP()
			time.Sleep(time.Second)
		}
	}()

	go func() {
		for {
			checkExec()
			time.Sleep(time.Second)
		}
	}()

}

// Periodically evaluate MQTT monitoring targets and issue alerts/recoveries as needed.
func evaluateMQTT() {

	monitorData.Lock()
	defer monitorData.Unlock()

	for topic, v := range monitorData.MQTT {

		if v.Deleted {
			continue
		}

		elapsed := time.Now().Sub(v.LastSeen).Seconds()

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

		// no custom timeout is configured, AvgTransmit is not yet established (NaN) -> skip
		if math.IsNaN(timeout) {
			continue
		}

		if elapsed > timeout {
			if v.Status != STATUS_ERROR {
				alert("MQTT", topic, STATUS_ERROR, v.LastSeen, fmt.Sprintf("timeout %.2fs", timeout))
				v.LastError = time.Now()
				v.Alerts++
			}
			v.Status = STATUS_ERROR
		} else if v.AvgTransmit > 0 && elapsed > v.AvgTransmit {
			v.Status = STATUS_WARN
		} else {
			if v.Status == STATUS_ERROR {
				alert("MQTT", topic, STATUS_OK, v.LastError, "")
			}
			v.Status = STATUS_OK
		}
		monitorData.MQTT[topic] = v
	}
}

// Matches and MQTT topic 'subject' to a topic pattern 'pattern', potentially containing wildcard ('#').
// Returns true if matches, false if not.
func matchMQTTTopic(pattern string, subject string) bool {
	sl := strings.Split(subject, "/")
	pl := strings.Split(pattern, "/")

	for i, _ := range sl {
		if len(pl)-1 < i {
			return false
		}
		if pl[i] == "#" {
			return true
		}
		if pl[i] != sl[i] {
			return false
		}
	}
	return true
}

// Periodically iterate through ping targets and perform check if required.
func checkPing() {
	monitorData.RLock()
	defer monitorData.RUnlock()
	for address, e := range monitorData.Ping {
		ts := e.Timestamp
		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(e.Interval) * time.Second).Before(time.Now()) {

			monitorData.RUnlock()
			r := ping(address)
			debug(fmt.Sprintf("Pinging %s: %t", address, r))
			monitorData.Lock()

			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()
				e.Errors = 0
				if e.Status == STATUS_ERROR {
					alert("Ping", e.Name, STATUS_OK, e.LastError, "")
				}
				e.Status = STATUS_OK
			} else {
				e.Errors++
				e.TotalError++
				e.LastError = time.Now()
				// First error will set STATUS_WARN, error will be triggered after the threshold
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				}
				if e.Status == STATUS_WARN && e.Errors >= e.Threshold {
					alert("Ping", e.Name, STATUS_ERROR, e.LastOK, "")
					e.Status = STATUS_ERROR
				}
			}
			monitorData.Unlock()
			monitorData.RLock()

		}

	}
}

// Periodically iterate through HTTP targets and perform check if required.
func checkHTTP() {
	monitorData.RLock()
	defer monitorData.RUnlock()
	for address, e := range monitorData.HTTP {
		ts := e.Timestamp
		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(e.Interval) * time.Second).Before(time.Now()) {

			monitorData.RUnlock()
			r, err, val := performHTTPCheck(address, e.Value, e.Timeout)
			debug(fmt.Sprintf("HTTP request %s: %t %s", address, r, err))
			monitorData.Lock()

			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()
				e.LastValue = val
				e.Errors = 0
				if e.Status == STATUS_ERROR {
					alert("HTTP", e.Name, STATUS_OK, e.LastError, "")
				}
				e.Status = STATUS_OK
			} else {
				e.Errors++
				e.TotalError++
				e.LastError = time.Now()
				e.LastErrorValue = err
				// First error will set STATUS_WARN, error will be triggered after the threshold
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				}
				if e.Status == STATUS_WARN && e.Errors >= e.Threshold {
					alert("HTTP", e.Name, STATUS_ERROR, e.LastOK, err)
					e.Status = STATUS_ERROR
				}

			}
			monitorData.Unlock()
			monitorData.RLock()
		}

	}
}

// Periodically iterate through exec targets and perform check if required.
func checkExec() {
	monitorData.RLock()
	defer monitorData.RUnlock()
	for command, e := range monitorData.Exec {
		ts := e.Timestamp
		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(e.Interval) * time.Second).Before(time.Now()) {

			monitorData.RUnlock()
			r := performExecCheck(command, e.Timeout)
			monitorData.Lock()

			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()

				e.Errors = 0
				if e.Status == STATUS_ERROR {
					alert("Exec", e.Name, STATUS_OK, e.LastError, "")
				}
				e.Status = STATUS_OK
			} else {
				e.Errors++
				e.TotalError++
				e.LastError = time.Now()

				// First error will set STATUS_WARN, error will be triggered after the threshold
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				}
				if e.Status == STATUS_WARN && e.Errors >= e.Threshold {
					alert("Exec", e.Name, STATUS_ERROR, e.LastOK, "")
					e.Status = STATUS_ERROR
				}

			}
			monitorData.Unlock()
			monitorData.RLock()
		}

	}
}

// Perform exec check for a single target.
// Return false in case of error or timeout.
func performExecCheck(command string, timeout int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(ctx, command)
	} else {
		cmd = exec.CommandContext(ctx, "sh", "-c", command)
	}
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
	resp, err := client.Get(url)
	if err != nil {
		errValue = err.Error()
	} else if resp.StatusCode != http.StatusOK {
		errValue = fmt.Sprintf("status code %d", resp.StatusCode)
	} else {
		defer resp.Body.Close()
		bodyBytes, bodyErr := ioutil.ReadAll(resp.Body)
		if bodyErr != nil {
			errValue = bodyErr.Error()
		} else {
			okValue = string(bodyBytes)
			if pattern != "" && !strings.Contains(okValue, pattern) {
				errValue = "Response does not match pattern"
			}
		}
	}

	return errValue == "", errValue, okValue
}

// Pings a host with a single packet (Linux and Windows).
// Returns false if ping exits with error code or with "Destination host unreachable".
// Returns true in case of successful ping.
func ping(host string) bool {
	cmd := exec.Command("ping", "-c", "1", host)
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "1", host)
	}
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

	tmpl, err := template.New("w").Funcs(template.FuncMap{
		"relaTime": relaTime,
		"escape":   template.HTMLEscaper,
		"json": func(i interface{}) string {
			s, _ := json.MarshalIndent(i, "", "\t")
			return string(s)
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
		}}).Parse(index_template)
	if err != nil {
		panic(err)
	}

	monitorData.RLock()
	logLock.RLock()

	tmpl.Execute(w,
		PageData{
			&monitorData,
			time.Now(),
			uptime,
			getConfig(),
			&logHistory})

	monitorData.RUnlock()
	logLock.RUnlock()
}

// Serves the api/stats page.
func serveAPIStats(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)
	o := 0
	e := 0
	monitorData.RLock()
	for k := range monitorData.MQTT {
		if !monitorData.MQTT[k].Deleted {
			if monitorData.MQTT[k].Status == STATUS_ERROR {
				e++
			} else {
				o++
			}
		}
	}
	for k := range monitorData.Ping {
		if monitorData.Ping[k].Status == STATUS_ERROR {
			e++
		} else {
			o++
		}
	}
	for k := range monitorData.HTTP {
		if monitorData.HTTP[k].Status == STATUS_ERROR {
			e++
		} else {
			o++
		}
	}
	for k := range monitorData.Exec {
		if monitorData.Exec[k].Status == STATUS_ERROR {
			e++
		} else {
			o++
		}
	}

	monitorData.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(StatsData{o, e})
}

// Serves the api/data page.
func serveAPIData(w http.ResponseWriter, r *http.Request) {
	debug("Web request " + r.RequestURI + " from " + r.RemoteAddr)
	monitorData.RLock()
	defer monitorData.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(monitorData)
}

// Reloads the config based on web request.
func reloadConfig(w http.ResponseWriter, r *http.Request) {
	log("Reloading config based on web request from " + r.RemoteAddr)
	loadConfig()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Deletes an item from monitoring. MQTT topics will not be deleted from the config due to possible wildcard subscriptions,
// but a Deleted flag will be set instead, and the topic will be deleted only upon configuration reload. Other targets will
// be deleted directly from the configuration and the monitoring data as well.
func deleteWebItem(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	debug(fmt.Sprintf("Web request %s from %s: %+v", r.RequestURI, r.RemoteAddr, r.Form))

	monitorData.Lock()
	defer monitorData.Unlock()

	if len(r.Form["type"]) > 0 && len(r.Form["name"]) > 0 {
		f := r.Form["name"][0]
		switch r.Form["type"][0] {
		case "mqtt":
			monitorData.MQTT[f].Deleted = true
		case "ping":
			for k, v := range getConfig().Monitor.Ping.Targets {
				if v.Address == f {
					configLock.Lock()
					config.Monitor.Ping.Targets = append(config.Monitor.Ping.Targets[:k], config.Monitor.Ping.Targets[k+1:]...)
					configLock.Unlock()
				}
			}
			delete(monitorData.Ping, f)
		case "http":
			for k, v := range getConfig().Monitor.HTTP.Targets {
				if v.Address == f {
					configLock.Lock()
					config.Monitor.HTTP.Targets = append(config.Monitor.HTTP.Targets[:k], config.Monitor.HTTP.Targets[k+1:]...)
					configLock.Unlock()
				}
			}
			delete(monitorData.HTTP, f)

		case "exec":
			for k, v := range getConfig().Monitor.Exec.Targets {
				if v.Command == f {
					configLock.Lock()
					config.Monitor.Exec.Targets = append(config.Monitor.Exec.Targets[:k], config.Monitor.Exec.Targets[k+1:]...)
					configLock.Unlock()
				}
			}
			delete(monitorData.Exec, f)
		}

	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Set parameters on monitored entries from web.
func configWebItem(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	debug(fmt.Sprintf("Web request %s from %s: %+v", r.RequestURI, r.RemoteAddr, r.Form))

	if len(r.Form["type"]) > 0 && len(r.Form["name"]) > 0 {
		f := r.Form["name"][0]
		switch r.Form["type"][0] {
		case "mqtt":
			if len(r.Form["timeout"]) > 0 {
				if v, err := strconv.ParseFloat(r.Form["timeout"][0], 64); err != nil {
					log("Unable to parse requested timeout: " + r.Form["timeout"][0])
				} else {
					monitorData.Lock()
					monitorData.MQTT[f].CustomTimeout = v
					monitorData.Unlock()
					evaluateMQTT()
				}
			}
		case "ping":
			if len(r.Form["interval"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["interval"][0], 10, 64); err != nil {
					log("Unable to parse requested interval: " + r.Form["interval"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.Ping.Interval)
					}
					monitorData.Lock()
					monitorData.Ping[f].Interval = int(v)
					monitorData.Unlock()
				}
			}
			if len(r.Form["threshold"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["threshold"][0], 10, 64); err != nil {
					log("Unable to parse requested threshold: " + r.Form["threshold"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.Ping.Threshold)
					}
					monitorData.Lock()
					monitorData.Ping[f].Threshold = int(v)
					monitorData.Unlock()
				}
			}

		case "http":
			if len(r.Form["interval"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["interval"][0], 10, 64); err != nil {
					log("Unable to parse requested interval: " + r.Form["interval"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.HTTP.Interval)
					}
					monitorData.Lock()
					monitorData.HTTP[f].Interval = int(v)
					monitorData.Unlock()
				}
			}
			if len(r.Form["timeout"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["timeout"][0], 10, 64); err != nil {
					log("Unable to parse requested timeout: " + r.Form["timeout"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.HTTP.Timeout)
					}
					monitorData.Lock()
					monitorData.HTTP[f].Timeout = int(v)
					monitorData.Unlock()
				}
			}
			if len(r.Form["threshold"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["threshold"][0], 10, 64); err != nil {
					log("Unable to parse requested threshold: " + r.Form["threshold"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.HTTP.Threshold)
					}
					monitorData.Lock()
					monitorData.HTTP[f].Threshold = int(v)
					monitorData.Unlock()
				}
			}

		case "exec":
			if len(r.Form["interval"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["interval"][0], 10, 64); err != nil {
					log("Unable to parse requested interval: " + r.Form["interval"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.Exec.Interval)
					}
					monitorData.Lock()
					monitorData.Exec[f].Interval = int(v)
					monitorData.Unlock()
				}
			}
			if len(r.Form["timeout"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["timeout"][0], 10, 64); err != nil {
					log("Unable to parse requested timeout: " + r.Form["timeout"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.Exec.Timeout)
					}
					monitorData.Lock()
					monitorData.Exec[f].Timeout = int(v)
					monitorData.Unlock()
				}
			}
			if len(r.Form["threshold"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["threshold"][0], 10, 64); err != nil {
					log("Unable to parse requested threshold: " + r.Form["threshold"][0])
				} else {
					if v == 0 {
						v = uint64(getConfig().Monitor.Exec.Threshold)
					}
					monitorData.Lock()
					monitorData.Exec[f].Threshold = int(v)
					monitorData.Unlock()
				}
			}

		}
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

	if getConfig().Alert.Telegram.Token != "" && getConfig().Alert.Telegram.Chat != 0 && tgbot != nil {
		if _, err := tgbot.Send(tgbotapi.NewMessage(getConfig().Alert.Telegram.Chat, s)); err != nil {
			log("Error sending to telegram: " + err.Error())
		}
	}
	if getConfig().Alert.Gotify.Token != "" && getConfig().Alert.Gotify.Server != "" {
		if _, err := http.PostForm(getConfig().Alert.Gotify.Server+"/message?token="+getConfig().Alert.Gotify.Token,
			url.Values{"message": {s}, "title": {"Janitor alert"}}); err != nil {
			log("Error in Gotify request: " + err.Error())
		}
	}
	if getConfig().Alert.Exec != "" {
		cmd := exec.Command("sh", "-c", getConfig().Alert.Exec)
		if runtime.GOOS == "windows" {
			cmd = exec.Command(getConfig().Alert.Exec)
		}
		cmd.Stdin = strings.NewReader(s)
		if err := cmd.Run(); err != nil {
			log("Error in executing " + getConfig().Alert.Exec + ": " + err.Error())
		}
	}

	// construct and post json payload for MQTT target
	if getConfig().Alert.MQTT.Server != "" && getConfig().Alert.MQTT.Topic != "" && alertMqttClient != nil {
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
		alertMqttClient.Publish(getConfig().Alert.MQTT.Topic, 0, false, b)
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
