package main

import (
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
	"github.com/gobuffalo/packr"
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
			Interval int
			Targets  []struct {
				Name     string
				Address  string
				Interval int
			}
		}
		HTTP struct {
			Interval int
			Timeout  int
			Targets  []struct {
				Name     string
				Address  string
				Value    string
				Interval int
				Timeout  int
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
	AvgTransmit   float64
	Timeout       float64
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
	Timestamp  time.Time
	Interval   int
}

type HTTPMonitorData struct {
	Name           string
	LastOK         time.Time
	LastError      time.Time
	LastValue      string
	LastErrorValue string
	Status         int32
	TotalOK        int64
	TotalError     int64
	Timestamp      time.Time
	Interval       int
	Timeout        int
}

// MonitorData stores the actual status data of the monitoring process.
type MonitorData struct {
	MQTT map[string]*MQTTMonitorData
	Ping map[string]*PingMonitorData
	HTTP map[string]*HTTPMonitorData
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
		HTTP: make(map[string]*HTTPMonitorData)}

	uptime = time.Now()

	monitorMqttClient mqtt.Client
	alertMqttClient   mqtt.Client
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
	if c.Monitor.HTTP.Interval == 0 {
		c.Monitor.HTTP.Interval = 60
	}
	if c.Monitor.HTTP.Timeout == 0 {
		c.Monitor.HTTP.Timeout = 5000
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

	// remove deleted MQTT targets
	monitorData.Lock()
	for k, _ := range monitorData.MQTT {
		if monitorData.MQTT[k].Deleted {
			delete(monitorData.MQTT, k)
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

		if elapsed > timeout && v.Status == STATUS_OK {
			alert("MQTT", topic, STATUS_ERROR, v.LastSeen, fmt.Sprintf("timeout %.2fs", timeout))
			v.LastError = time.Now()
			v.Alerts++
			v.Status = STATUS_ERROR
		} else if elapsed < timeout && v.Status == STATUS_ERROR {
			alert("MQTT", topic, STATUS_OK, v.LastError, "")
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
	for _, target := range getConfig().Monitor.Ping.Targets {

		monitorData.Lock()

		e, ok := monitorData.Ping[target.Address]
		if !ok {
			monitorData.Ping[target.Address] = &PingMonitorData{}
			e = monitorData.Ping[target.Address]
			e.Name = target.Name
		}

		ts := e.Timestamp

		i := e.Interval
		if i == 0 {
			i = target.Interval
		}
		if i == 0 {
			i = getConfig().Monitor.Ping.Interval
		}
		e.Interval = i

		monitorData.Unlock()

		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(i) * time.Second).Before(time.Now()) {

			r := ping(target.Address)
			debug(fmt.Sprintf("Pinging %s: %t", target.Address, r))

			monitorData.Lock()
			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()
				if e.Status == STATUS_ERROR {
					alert("Ping", target.Name, STATUS_OK, e.LastError, "")
				}
				e.Status = STATUS_OK
			} else {
				e.TotalError++
				e.LastError = time.Now()
				// First error will set STATUS_WARN, the second will set STATUS_ERROR
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				} else if e.Status == STATUS_WARN {
					alert("Ping", target.Name, STATUS_ERROR, e.LastOK, "")
					e.Status = STATUS_ERROR
				}
			}
			monitorData.Unlock()

		}

	}
}

// Periodically iterate through HTTP targets and perform check if required.
func checkHTTP() {
	for _, target := range getConfig().Monitor.HTTP.Targets {

		monitorData.Lock()

		e, ok := monitorData.HTTP[target.Address]
		if !ok {
			monitorData.HTTP[target.Address] = &HTTPMonitorData{}
			e = monitorData.HTTP[target.Address]
			e.Name = target.Name
		}

		ts := e.Timestamp
		i := e.Interval
		timeout := e.Timeout

		if i == 0 {
			i = target.Interval
		}
		if i == 0 {
			i = getConfig().Monitor.Ping.Interval
		}

		if timeout == 0 {
			timeout = target.Timeout
		}
		if timeout == 0 {
			timeout = getConfig().Monitor.HTTP.Timeout
		}

		e.Interval = i
		e.Timeout = timeout
		monitorData.Unlock()

		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(i) * time.Second).Before(time.Now()) {

			r, err, val := performHTTPCheck(target.Address, target.Value, timeout)
			debug(fmt.Sprintf("HTTP request %s: %t %s", target.Address, r, err))

			monitorData.Lock()
			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()
				e.LastValue = val
				if e.Status == STATUS_ERROR {
					alert("HTTP", target.Name, STATUS_OK, e.LastError, "")
				}
				e.Status = STATUS_OK
			} else {
				e.TotalError++
				e.LastError = time.Now()
				e.LastErrorValue = err
				// First error will set STATUS_WARN, the second will set STATUS_ERROR
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				} else if e.Status == STATUS_WARN {
					alert("HTTP", target.Name, STATUS_ERROR, e.LastOK, err)
					e.Status = STATUS_ERROR
				}

			}
			monitorData.Unlock()
		}

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

	box := packr.NewBox("./templates")
	s, err := box.FindString("index.html")
	if err != nil {
		panic(err)
	}

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
		}}).Parse(s)
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
					monitorData.Lock()
					monitorData.Ping[f].Interval = int(v)
					monitorData.Unlock()
				}
			}
		case "http":
			if len(r.Form["interval"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["interval"][0], 10, 64); err != nil {
					log("Unable to parse requested interval: " + r.Form["interval"][0])
				} else {
					monitorData.Lock()
					monitorData.HTTP[f].Interval = int(v)
					monitorData.Unlock()
				}
			}
			if len(r.Form["timeout"]) > 0 {
				if v, err := strconv.ParseUint(r.Form["timeout"][0], 10, 64); err != nil {
					log("Unable to parse requested timeout: " + r.Form["timeout"][0])
				} else {
					monitorData.Lock()
					monitorData.HTTP[f].Timeout = int(v)
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
