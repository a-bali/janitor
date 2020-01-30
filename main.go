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
	FirstSeen   time.Time
	LastSeen    time.Time
	LastError   time.Time
	LastPayload string
	History     []TimedEntry
	AvgTransmit float64
	Timeout     float64
	Status      int32
	Samples     int64
	Alerts      int64
	Deleted     bool
}

type PingMonitorData struct {
	Address    string
	LastOK     time.Time
	LastError  time.Time
	Status     int32
	TotalOK    int64
	TotalError int64
	Timestamp  time.Time
	Interval   int
}

type HTTPMonitorData struct {
	Address        string
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

	uptime     = time.Now()
	mqttClient mqtt.Client
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
	panic(http.ListenAndServe(fmt.Sprintf("%s:%d", getConfig().Web.Host, getConfig().Web.Port), nil))
}

// Loads or reloads the configuration and initializes MQTT and Telegram connections accordingly.
func loadConfig() {
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

	if newconfig.LogSize == 0 {
		newconfig.LogSize = MAXLOGSIZE
	}
	if newconfig.Web.Port == 0 {
		newconfig.Web.Port = 8080
	}
	if newconfig.Monitor.MQTT.History == 0 {
		newconfig.Monitor.MQTT.History = 10
	}
	if newconfig.Monitor.MQTT.Port == 0 {
		newconfig.Monitor.MQTT.Port = 1883
	}
	if newconfig.Monitor.MQTT.StandardTimeout == 0 {
		newconfig.Monitor.MQTT.StandardTimeout = 1.5
	}
	if newconfig.Monitor.Ping.Interval == 0 {
		newconfig.Monitor.Ping.Interval = 60
	}
	if newconfig.Monitor.HTTP.Interval == 0 {
		newconfig.Monitor.HTTP.Interval = 60
	}
	if newconfig.Monitor.HTTP.Timeout == 0 {
		newconfig.Monitor.HTTP.Timeout = 5000
	}

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
		if mqttClient != nil && mqttClient.IsConnected() {
			mqttClient.Disconnect(1)
			debug("Disconnected from MQTT")
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

		mqttClient = mqtt.NewClient(opts)
		if token := mqttClient.Connect(); token.Wait() && token.Error() != nil {
			log("Unable to connect to MQTT: " + token.Error().Error())
		} else {
			log("Connected to MQTT server at " + opts.Servers[0].String())
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
		timeout := v.AvgTransmit * getConfig().Monitor.MQTT.StandardTimeout

		// if custom timeout is configured, use that instead of the standard
		for _, t := range getConfig().Monitor.MQTT.Targets {
			if matchMQTTTopic(t.Topic, topic) && t.Timeout > 0 {
				timeout = float64(t.Timeout)
				break
			}
		}

		// no custom timeout is configured, AvgTransmit is not yet established (NaN) -> skip
		if math.IsNaN(timeout) {
			continue
		}
		// Store calculated timeout for showing on web
		v.Timeout = timeout

		if elapsed > timeout && v.Status == STATUS_OK {
			alert(fmt.Sprintf("⚠ MQTT ERROR: %s last seen %s ago (timeout %.2fs)", topic, relaTime(v.LastSeen), timeout))
			v.LastError = time.Now()
			v.Alerts++
			v.Status = STATUS_ERROR
		} else if elapsed < timeout && v.Status == STATUS_ERROR {
			alert(fmt.Sprintf("✓ MQTT OK for %s, in error since %s ago", topic, relaTime(v.LastError)))
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

		e, ok := monitorData.Ping[target.Name]
		if !ok {
			monitorData.Ping[target.Name] = &PingMonitorData{}
			e = monitorData.Ping[target.Name]
			e.Address = target.Address
		}

		ts := e.Timestamp
		i := e.Interval
		monitorData.Unlock()

		if i == 0 {
			i = target.Interval
		}
		if i == 0 {
			i = getConfig().Monitor.Ping.Interval
		}

		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(i) * time.Second).Before(time.Now()) {

			r := ping(target.Address)
			debug(fmt.Sprintf("Pinging %s: %t", e.Address, r))

			monitorData.Lock()
			e.Interval = i
			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()
				if e.Status == STATUS_ERROR {
					alert("✓ Ping OK for " + target.Name + ", in error since " + relaTime(e.LastError) + " ago")
				}
				e.Status = STATUS_OK
			} else {
				e.TotalError++
				e.LastError = time.Now()
				// First error will set STATUS_WARN, the second will set STATUS_ERROR
				if e.Status == STATUS_OK {
					e.Status = STATUS_WARN
				} else if e.Status == STATUS_WARN {
					alert("⚠ Ping ERROR for " + target.Name + ", last seen " + relaTime(e.LastOK) + " ago")
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

		e, ok := monitorData.HTTP[target.Name]
		if !ok {
			monitorData.HTTP[target.Name] = &HTTPMonitorData{}
			e = monitorData.HTTP[target.Name]
			e.Address = target.Address
		}

		ts := e.Timestamp
		i := e.Interval
		timeout := e.Timeout
		monitorData.Unlock()

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
		// proceed only if last check (regardless of outcome) was before now minus the configured interval
		if ts.Add(time.Duration(i) * time.Second).Before(time.Now()) {

			r, err, val := performHTTPCheck(target.Address, target.Value, timeout)
			debug(fmt.Sprintf("HTTP request %s: %t %s", e.Address, r, err))

			monitorData.Lock()
			e.Interval = i
			e.Timeout = timeout
			e.Timestamp = time.Now()
			if r {
				e.TotalOK++
				e.LastOK = time.Now()
				e.LastValue = val
				if e.Status == STATUS_ERROR {
					alert("✓ HTTP OK for " + target.Name + ", in error since " + relaTime(e.LastError) + " ago")
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
					alert("⚠ HTTP ERROR for " + target.Name + ", last seen " + relaTime(e.LastOK) + " ago (" + err + ")")
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
	cmd := exec.Command("x")
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "1", host)
	} else {
		cmd = exec.Command("ping", "-c", "1", host)
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
			for k, v := range monitorData.Ping {
				if v.Address == f {
					delete(monitorData.Ping, k)
				}
			}

		case "http":
			for k, v := range getConfig().Monitor.HTTP.Targets {
				if v.Address == f {
					configLock.Lock()
					config.Monitor.HTTP.Targets = append(config.Monitor.HTTP.Targets[:k], config.Monitor.HTTP.Targets[k+1:]...)
					configLock.Unlock()
				}
			}
			for k, v := range monitorData.HTTP {
				if v.Address == f {
					delete(monitorData.HTTP, k)
				}
			}
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Published an alert message via the methods configured.
func alert(s string) {
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
